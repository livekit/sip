// Copyright 2025 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cloudagents

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"
)

func TestUploadSource(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "tarball-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	subDir := filepath.Join(tmpDir, "subdir")
	err = os.MkdirAll(subDir, 0755)
	require.NoError(t, err)

	normalFiles := []string{
		filepath.Join(subDir, "normal1.txt"),
		filepath.Join(subDir, "normal2.txt"),
		filepath.Join(tmpDir, "root.txt"),
	}
	for _, path := range normalFiles {
		err = os.WriteFile(path, []byte("normal content"), 0644)
		require.NoError(t, err)
	}

	largeFile := filepath.Join(tmpDir, "large.bin")
	f, err := os.Create(largeFile)
	require.NoError(t, err)

	fileSize := int64(1024 * 1024 * 1024) // 1GB
	err = f.Truncate(fileSize)
	require.NoError(t, err)
	f.Close()

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NotZero(t, len(body))
		require.Greater(t, fileSize, int64(len(body)))
		w.WriteHeader(http.StatusOK)
	}))
	defer mockServer.Close()

	err = uploadSource(os.DirFS(tmpDir), mockServer.URL, nil, []string{})
	require.NoError(t, err)
}

func TestUploadSourcePost(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "tarball-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	subDir := filepath.Join(tmpDir, "subdir")
	err = os.MkdirAll(subDir, 0755)
	require.NoError(t, err)

	normalFiles := []string{
		filepath.Join(subDir, "normal1.txt"),
		filepath.Join(subDir, "normal2.txt"),
		filepath.Join(tmpDir, "root.txt"),
	}
	for _, path := range normalFiles {
		err = os.WriteFile(path, []byte("normal content"), 0644)
		require.NoError(t, err)
	}

	largeFile := filepath.Join(tmpDir, "large.bin")
	f, err := os.Create(largeFile)
	require.NoError(t, err)

	fileSize := int64(1024 * 1024 * 1024) // 1GB
	err = f.Truncate(fileSize)
	require.NoError(t, err)
	f.Close()

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := r.ParseMultipartForm(1024 * 1024 * 1024)
		require.NoError(t, err)
		require.NotZero(t, len(r.MultipartForm.File["file"]))
		require.Equal(t, "upload.tar.gz", r.MultipartForm.File["file"][0].Filename)
		require.Greater(t, fileSize, r.MultipartForm.File["file"][0].Size)
		w.WriteHeader(http.StatusCreated)
	}))
	defer mockServer.Close()

	err = uploadSource(os.DirFS(tmpDir), "", &livekit.PresignedPostRequest{
		Url:    mockServer.URL,
		Values: map[string]string{},
	}, []string{})
	require.NoError(t, err)
}

func TestUploadSourceFilePermissions(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "tarball-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	subDir := filepath.Join(tmpDir, "subdir")
	err = os.MkdirAll(subDir, 0755)
	require.NoError(t, err)

	normalFiles := []string{
		filepath.Join(subDir, "normal1.txt"),
		filepath.Join(subDir, "normal2.txt"),
	}
	for _, path := range normalFiles {
		err = os.WriteFile(path, []byte("normal content"), 0644)
		require.NoError(t, err)
	}

	restrictedFile := filepath.Join(subDir, "restricted.txt")
	err = os.WriteFile(restrictedFile, []byte("restricted content"), 0000)
	require.NoError(t, err)

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mockServer.Close()

	// Windows permissions model does not support unix-like file mode permissions
	// and creating a file you can't read requires modifying Access Control Lists:
	//
	//   https://learn.microsoft.com/en-us/windows/win32/secauthz/access-control-lists
	//
	if runtime.GOOS != "windows" {
		err = uploadSource(os.DirFS(tmpDir), mockServer.URL, nil, []string{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "permission denied")
	}

	err = os.Remove(restrictedFile)
	require.NoError(t, err)

	err = uploadSource(os.DirFS(tmpDir), mockServer.URL, nil, []string{})
	require.NoError(t, err)
}

type tarContents struct {
	Name       string
	Size       int64
	IsDir      bool
	IsSymlink  bool
	LinkTarget string
}

func readTarContents(t *testing.T, tarData []byte) []tarContents {
	t.Helper()
	var contents []tarContents

	gzipReader, err := gzip.NewReader(bytes.NewReader(tarData))
	require.NoError(t, err)
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)

		content := tarContents{
			Name:       header.Name,
			Size:       header.Size,
			IsDir:      header.Typeflag == tar.TypeDir,
			IsSymlink:  header.Typeflag == tar.TypeSymlink,
			LinkTarget: header.Linkname,
		}
		contents = append(contents, content)
	}
	return contents
}

func TestUploadSourceDotfiles(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "tarball-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	subDir := filepath.Join(tmpDir, "src")
	err = os.MkdirAll(subDir, 0755)
	require.NoError(t, err)

	files := []struct {
		path    string
		content string
	}{
		{filepath.Join(tmpDir, "regular.txt"), "regular file"},
		{filepath.Join(subDir, "code.go"), "package main"},
	}
	for _, f := range files {
		err = os.WriteFile(f.path, []byte(f.content), 0644)
		require.NoError(t, err)
	}

	dotfiles := []struct {
		path    string
		content string
	}{
		{filepath.Join(tmpDir, ".gitignore"), ".env.*\nnode_modules/"},
		{filepath.Join(tmpDir, ".env"), "SECRET=123"},
		{filepath.Join(tmpDir, ".config"), "config data"},
		{filepath.Join(subDir, ".DS_Store"), "mac file"},
	}
	for _, f := range dotfiles {
		err = os.WriteFile(f.path, []byte(f.content), 0644)
		require.NoError(t, err)
	}

	// symlinks
	err = os.Symlink(files[0].path, filepath.Join(tmpDir, "link_to_regular.txt"))
	require.NoError(t, err)
	err = os.Symlink(dotfiles[2].path, filepath.Join(tmpDir, ".link_to_config"))
	require.NoError(t, err)

	nodeModules := filepath.Join(tmpDir, "node_modules")
	err = os.MkdirAll(nodeModules, 0755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(nodeModules, "package.json"), []byte("{}"), 0644)
	require.NoError(t, err)

	var tarBuffer bytes.Buffer
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(&tarBuffer, r.Body)
		require.NoError(t, err)
		w.WriteHeader(http.StatusOK)
	}))
	defer mockServer.Close()

	err = uploadSource(os.DirFS(tmpDir), mockServer.URL, nil, []string{})
	require.NoError(t, err)

	contents := readTarContents(t, tarBuffer.Bytes())

	expectedFiles := map[string]struct{}{
		"regular.txt": {},
		"src/code.go": {},
		".config":     {},
		// symlink files are excluded
		// "link_to_regular.txt": {},
		// ".link_to_config":     {},
	}

	excludedFiles := map[string]struct{}{
		".env":          {},
		".gitignore":    {},
		"node_modules/": {},
		".DS_Store":     {},
	}

	for _, content := range contents {
		if _, ok := expectedFiles[content.Name]; ok {
			delete(expectedFiles, content.Name)

			switch content.Name {
			case "link_to_regular.txt":
				require.Equal(t, int64(len("regular file")), content.Size)
				require.False(t, content.IsSymlink)
			case ".link_to_config":
				require.Equal(t, int64(len("config data")), content.Size)
				require.False(t, content.IsSymlink)
			}
		}
		_, shouldBeExcluded := excludedFiles[content.Name]
		require.False(t, shouldBeExcluded, "excluded file was found in tar: %s", content.Name)
	}

	require.Empty(t, expectedFiles, "some expected files were not found in tar: %v", expectedFiles)
}

func TestUploadSourceDeepDirectories(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "tarball-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dirs := []string{
		"level1",
		path.Join("level1", "level2"),
		path.Join("level1", "level2", "level3"),
		path.Join("level1", "level2", "level3", "level4"),
	}

	for _, dir := range dirs {
		err = os.MkdirAll(path.Join(tmpDir, dir), 0755)
		require.NoError(t, err)
		initPath := path.Join(tmpDir, dir, "__init__.py")
		err = os.WriteFile(initPath, []byte(""), 0644)
		require.NoError(t, err)
	}

	files := []struct {
		path    string
		content string
	}{
		{path.Join(tmpDir, "root.txt"), "root file"},
		{path.Join(tmpDir, "level1", "level1.txt"), "level 1 file"},
		{path.Join(tmpDir, "level1", "level2", "level2.txt"), "level 2 file"},
		{path.Join(tmpDir, "level1", "level2", "level3", "level3.txt"), "level 3 file"},
		{path.Join(tmpDir, "level1", "level2", "level3", "level4", "level4.txt"), "level 4 file"},
	}

	for _, f := range files {
		err = os.WriteFile(f.path, []byte(f.content), 0644)
		require.NoError(t, err)
	}

	var tarBuffer bytes.Buffer
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(&tarBuffer, r.Body)
		require.NoError(t, err)
		w.WriteHeader(http.StatusOK)
	}))
	defer mockServer.Close()

	err = uploadSource(os.DirFS(tmpDir), mockServer.URL, nil, []string{})
	require.NoError(t, err)

	contents := readTarContents(t, tarBuffer.Bytes())

	for _, dir := range dirs {
		found := false
		for _, content := range contents {
			if content.Name == filepath.ToSlash(dir)+"/" && content.IsDir {
				found = true
				break
			}
		}
		require.True(t, found, "directory not found in tar: %s", dir)
	}

	for _, f := range files {
		found := false
		relPath, err := filepath.Rel(tmpDir, f.path)
		require.NoError(t, err)
		for _, content := range contents {
			if filepath.ToSlash(content.Name) == filepath.ToSlash(relPath) {
				found = true
				require.Equal(t, int64(len(f.content)), content.Size, "incorrect file size for %s", relPath)
				require.False(t, content.IsDir, "file marked as directory: %s", relPath)
				break
			}
		}
		require.True(t, found, "file not found in tar: %s", relPath)
	}

	for _, dir := range dirs {
		initPath := filepath.Join(dir, "__init__.py")
		found := false
		for _, content := range contents {
			if filepath.ToSlash(content.Name) == filepath.ToSlash(initPath) {
				found = true
				require.Equal(t, int64(0), content.Size, "incorrect file size for %s", initPath)
				require.False(t, content.IsDir, "file marked as directory: %s", initPath)
				break
			}
		}
		require.True(t, found, "__init__.py not found in tar: %s", initPath)
	}
}

func TestUploadSourceWithDockerignore(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "dockerignore-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dockerignoreContent := `node_modules/
__pycache__/
.venv/
venv/
.env
.git/
*.log`

	err = os.WriteFile(filepath.Join(tmpDir, ".dockerignore"), []byte(dockerignoreContent), 0644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tmpDir, "main.py"), []byte("print('Hello World')"), 0644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tmpDir, "requirements.txt"), []byte("requests==2.28.0"), 0644)
	require.NoError(t, err)

	excludedDirs := []string{
		"__pycache__",
		".venv",
		"venv",
		".git",
	}

	for _, dir := range excludedDirs {
		err = os.MkdirAll(filepath.Join(tmpDir, dir), 0755)
		require.NoError(t, err)

		err = os.WriteFile(filepath.Join(tmpDir, dir, "test.txt"), []byte("should be excluded"), 0644)
		require.NoError(t, err)
	}

	err = os.WriteFile(filepath.Join(tmpDir, ".env"), []byte("SECRET_KEY=12345"), 0644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tmpDir, "app.log"), []byte("log content"), 0644)
	require.NoError(t, err)

	var tarBuffer bytes.Buffer
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(&tarBuffer, r.Body)
		require.NoError(t, err)
		w.WriteHeader(http.StatusOK)
	}))
	defer mockServer.Close()

	err = uploadSource(os.DirFS(tmpDir), mockServer.URL, nil, []string{})
	require.NoError(t, err)

	contents := readTarContents(t, tarBuffer.Bytes())

	expectedFiles := map[string]bool{
		"main.py":          false,
		"requirements.txt": false,
	}

	for _, content := range contents {
		if _, exists := expectedFiles[content.Name]; exists {
			expectedFiles[content.Name] = true
		}
	}

	for file, found := range expectedFiles {
		require.True(t, found, "included file not found in tar: %s", file)
	}

	for _, content := range contents {
		for _, excludedDir := range excludedDirs {
			require.False(t, strings.HasPrefix(content.Name, excludedDir),
				"excluded directory content found in tar: %s", content.Name)
		}

		require.NotEqual(t, ".env", content.Name, ".env file should not be in tar")
		require.NotEqual(t, "app.log", content.Name, "*.log file should not be in tar")
	}

	for _, content := range contents {
		require.NotEqual(t, ".dockerignore", content.Name, ".dockerignore file should not be in tar")
	}
}

func TestUploadSourceWithPipPythonProject(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "pip-python-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	err = os.WriteFile(filepath.Join(tmpDir, "main.py"), []byte("print('Hello from pip project')"), 0644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tmpDir, "requirements.txt"), []byte("requests==2.28.0\nflask==2.3.0"), 0644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tmpDir, "setup.py"), []byte("from setuptools import setup\nsetup(name='myapp')"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "Dockerfile"), []byte("Dockerfile content"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "Dockerfile.prod"), []byte("Dockerfile prod content"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "other-Dockerfile"), []byte("other Dockerfile content"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, ".dockerignore"), []byte(pipDockerIgnoreContent), 0644)
	require.NoError(t, err)

	excludedDirsCheck := []string{
		"__pycache__",
		".venv",
		"venv",
		".git",
	}

	for _, dir := range excludedDirsCheck {
		err = os.MkdirAll(filepath.Join(tmpDir, dir), 0755)
		require.NoError(t, err)

		err = os.WriteFile(filepath.Join(tmpDir, dir, "test.txt"), []byte("should be excluded"), 0644)
		require.NoError(t, err)
	}

	// create a subdir with excluded files
	err = os.MkdirAll(filepath.Join(tmpDir, "some-dir"), 0755)
	require.NoError(t, err)
	err = os.MkdirAll(filepath.Join(tmpDir, "some-dir", ".venv"), 0755)
	require.NoError(t, err)
	err = os.MkdirAll(filepath.Join(tmpDir, "some-dir", "venv"), 0755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "some-dir", ".env"), []byte("SECRET_KEY=12345"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "some-dir", "app.log"), []byte("log content"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "some-dir", "main.pyc"), []byte("compiled python"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "some-dir", "livekit.toml"), []byte("some toml content"), 0644)
	require.NoError(t, err)

	// excluded files
	err = os.WriteFile(filepath.Join(tmpDir, ".env"), []byte("SECRET_KEY=12345"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "app.log"), []byte("log content"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "main.pyc"), []byte("compiled python"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "livekit.toml"), []byte("some toml content"), 0644)
	require.NoError(t, err)

	var tarBuffer bytes.Buffer
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(&tarBuffer, r.Body)
		require.NoError(t, err)
		w.WriteHeader(http.StatusOK)
	}))
	defer mockServer.Close()

	err = uploadSource(os.DirFS(tmpDir), mockServer.URL, nil, []string{"**/livekit.toml"})
	require.NoError(t, err)

	contents := readTarContents(t, tarBuffer.Bytes())

	expectedFiles := map[string]bool{
		"main.py":          false,
		"requirements.txt": false,
		"setup.py":         false,
		"Dockerfile":       false,
		"Dockerfile.prod":  false,
		"other-Dockerfile": false,
	}

	for _, content := range contents {
		if _, exists := expectedFiles[content.Name]; exists {
			expectedFiles[content.Name] = true
		}
	}

	for file, found := range expectedFiles {
		require.True(t, found, "included file not found in tar: %s", file)
	}

	for _, content := range contents {
		for _, excludedDir := range excludedDirsCheck {
			require.False(t, strings.HasPrefix(content.Name, excludedDir),
				"excluded directory content found in tar: %s", content.Name)
		}

		excludedFiles := []string{
			".env",
			"app.log",
			"main.pyc",
			".dockerignore",
			"some-dir/.env",
			"some-dir/.venv",
			"some-dir/venv",
			"some-dir/app.log",
			"some-dir/main.pyc",
			"some-dir/livekit.toml",
		}

		for _, excludedFile := range excludedFiles {
			require.NotEqual(t, excludedFile, content.Name,
				"excluded file should not be in tar: %s", content.Name)
		}
	}

}

func TestUploadSourceWithUvPythonProject(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "uv-python-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	err = os.WriteFile(filepath.Join(tmpDir, "main.py"), []byte("print('Hello from uv project')"), 0644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tmpDir, "pyproject.toml"), []byte("[project]\nname = 'myapp'\nversion = '0.1.0'"), 0644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tmpDir, "utils.py"), []byte("def helper():\n    pass"), 0644)
	require.NoError(t, err)

	err = os.MkdirAll(filepath.Join(tmpDir, "some-dir"), 0755)
	require.NoError(t, err)
	err = os.MkdirAll(filepath.Join(tmpDir, "some-dir", ".venv"), 0755)
	require.NoError(t, err)
	err = os.MkdirAll(filepath.Join(tmpDir, "some-dir", "venv"), 0755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "some-dir", ".env"), []byte("SECRET_KEY=12345"), 0644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tmpDir, ".env"), []byte("SECRET_KEY=12345"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "app.log"), []byte("log content"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "main.pyc"), []byte("compiled python"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "Dockerfile"), []byte("Dockerfile content"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "Dockerfile.dev"), []byte("Dockerfile dev content"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, ".dockerignore"), []byte(uvDockerIgnoreContent), 0644)
	require.NoError(t, err)

	var tarBuffer bytes.Buffer
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(&tarBuffer, r.Body)
		require.NoError(t, err)
		w.WriteHeader(http.StatusOK)
	}))
	defer mockServer.Close()

	err = uploadSource(os.DirFS(tmpDir), mockServer.URL, nil, []string{"**/livekit.toml"})
	require.NoError(t, err)

	contents := readTarContents(t, tarBuffer.Bytes())

	expectedFiles := map[string]bool{
		"main.py":        false,
		"pyproject.toml": false,
		"utils.py":       false,
		"Dockerfile":     false,
		"Dockerfile.dev": false,
	}

	for _, content := range contents {
		if _, exists := expectedFiles[content.Name]; exists {
			expectedFiles[content.Name] = true
		}
	}

	for file, found := range expectedFiles {
		require.True(t, found, "included file not found in tar: %s", file)
	}

	for _, content := range contents {
		excludedDirs := []string{
			"__pycache__",
			".venv",
			"venv",
			".git",
		}

		for _, excludedDir := range excludedDirs {
			require.False(t, strings.HasPrefix(content.Name, excludedDir),
				"excluded directory content found in tar: %s", content.Name)
		}

		excludedFiles := []string{
			".env",
			"app.log",
			"main.pyc",
			".dockerignore",
			"some-dir/.env",
			"some-dir/.venv",
			"some-dir/venv",
			"some-dir/app.log",
			"some-dir/main.pyc",
			"some-dir/livekit.toml",
		}

		for _, excludedFile := range excludedFiles {
			require.NotEqual(t, excludedFile, content.Name,
				"excluded file should not be in tar: %s", content.Name)
		}
	}
}

func TestUploadSourceWithNodeProject(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "node-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	err = os.WriteFile(filepath.Join(tmpDir, "index.js"), []byte("console.log('Hello from Node project')"), 0644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tmpDir, "package.json"), []byte(`{"name": "myapp", "version": "1.0.0"}`), 0644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tmpDir, "utils.js"), []byte("function helper() {\n    console.log('helper');\n}"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "Dockerfile"), []byte("Dockerfile content"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "Dockerfile.dev"), []byte("Dockerfile dev content"), 0644)
	require.NoError(t, err)

	err = os.MkdirAll(filepath.Join(tmpDir, "some-dir"), 0755)
	require.NoError(t, err)
	err = os.MkdirAll(filepath.Join(tmpDir, "some-dir", "node_modules"), 0755)
	require.NoError(t, err)
	err = os.MkdirAll(filepath.Join(tmpDir, "some-dir", "dist"), 0755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "some-dir", ".env"), []byte("NODE_ENV=development"), 0644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tmpDir, ".env"), []byte("NODE_ENV=development"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "app.log"), []byte("log content"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "npm-debug.log"), []byte("npm debug"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, ".DS_Store"), []byte("mac file"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, ".dockerignore"), []byte(nodeJsDockerIgnoreContent), 0644)
	require.NoError(t, err)

	var tarBuffer bytes.Buffer
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(&tarBuffer, r.Body)
		require.NoError(t, err)
		w.WriteHeader(http.StatusOK)
	}))
	defer mockServer.Close()

	err = uploadSource(os.DirFS(tmpDir), mockServer.URL, nil, []string{"**/livekit.toml"})
	require.NoError(t, err)

	contents := readTarContents(t, tarBuffer.Bytes())

	expectedFiles := map[string]bool{
		"index.js":       false,
		"package.json":   false,
		"utils.js":       false,
		"Dockerfile":     false,
		"Dockerfile.dev": false,
	}

	for _, content := range contents {
		if _, exists := expectedFiles[content.Name]; exists {
			expectedFiles[content.Name] = true
		}
	}

	for file, found := range expectedFiles {
		require.True(t, found, "included file not found in tar: %s", file)
	}

	for _, content := range contents {
		excludedDirs := []string{
			"node_modules",
			"dist",
			"build",
			"coverage",
			".git",
		}

		for _, excludedDir := range excludedDirs {
			require.False(t, strings.HasPrefix(content.Name, excludedDir),
				"excluded directory content found in tar: %s", content.Name)
		}

		excludedFiles := []string{
			".env",
			"app.log",
			"npm-debug.log",
			".DS_Store",
			".dockerignore",
		}

		for _, excludedFile := range excludedFiles {
			require.NotEqual(t, excludedFile, content.Name,
				"excluded file should not be in tar: %s", content.Name)
		}

		excludedPatterns := []string{
			"node_modules",
			"dist",
			"build",
			"coverage",
			".git",
		}

		for _, pattern := range excludedPatterns {
			if strings.Contains(content.Name, pattern) {
				t.Errorf("excluded pattern '%s' found in tar content: %s", pattern, content.Name)
			}
		}
	}
}

var pipDockerIgnoreContent = `
# Python bytecode and artifacts
**/__pycache__/
**/*.py[cod]
**/*.pyo
**/*.pyd
**/*.egg-info/
**/dist/
**/build/

# Virtual environments
**/.venv/
**/venv/

# Caches and test output
**/.cache/
**/.pytest_cache/
**/.ruff_cache/
**/coverage/

# Logs and temp files
**/*.log
**/*.gz
**/*.tgz
**/.tmp
**/.cache

# Environment variables
**/.env
**/.env.*

# VCS, editor, OS
.git
.gitignore
.gitattributes
.github/
.idea/
.vscode/
.DS_Store

# Project docs and misc
README.md
LICENSE

# Project tests
test/
tests/
eval/
evals/`

var uvDockerIgnoreContent = `
# Python bytecode and artifacts
**/__pycache__/
**/*.py[cod]
**/*.pyo
**/*.pyd
**/*.egg-info/
**/dist/
**/build/

# Virtual environments
**/.venv/
**/venv/

# Caches and test output
**/.cache/
**/.pytest_cache/
**/.ruff_cache/
**/coverage/

# Logs and temp files
**/*.log
**/*.gz
**/*.tgz
**/.tmp
**/.cache

# Environment variables
**/.env
**/.env.*

# VCS, editor, OS
.git
.gitignore
.gitattributes
.github/
.idea/
.vscode/
.DS_Store

# Project docs and misc
README.md
LICENSE

# Project tests
test/
tests/
eval/
evals/
`

var nodeJsDockerIgnoreContent = `
# Node.js dependencies
**/node_modules
**/npm-debug.log
**/yarn-error.log
**/pnpm-debug.log

# Build outputs
**/dist
**/build
**/coverage

# Local environment & config files
**/.env
**/.env.local
.DS_Store

# Logs & temp files
**/*.log
**/*.gz
**/*.tgz
**/.tmp
**/.cache

# Docker artifacts
Dockerfile*
.dockerignore

# Git & Editor files
.git
.gitignore
.idea
.vscode
`
