package lksdk

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestChunkUtf8String(t *testing.T) {
	t.Run("empty string", func(t *testing.T) {
		chunks := chunkUtf8String("")
		require.Zero(t, len(chunks))
	})

	t.Run("string shorter than chunk size", func(t *testing.T) {
		testString := strings.Repeat("a", 1000)
		chunks := chunkUtf8String(testString)

		require.Len(t, chunks, 1)
		require.Equal(t, testString, string(chunks[0]))
	})

	t.Run("string exactly at chunk size", func(t *testing.T) {
		testString := strings.Repeat("a", STREAM_CHUNK_SIZE)
		chunks := chunkUtf8String(testString)

		require.Len(t, chunks, 1)
		require.Equal(t, testString, string(chunks[0]))
	})

	t.Run("ascii string longer than chunk size", func(t *testing.T) {
		testString := strings.Repeat("a", STREAM_CHUNK_SIZE*2+100)
		chunks := chunkUtf8String(testString)

		require.Len(t, chunks, 3)

		// Reconstruct the original string and check
		var reconstructed []byte
		for _, chunk := range chunks {
			reconstructed = append(reconstructed, chunk...)
		}

		require.Equal(t, testString, string(reconstructed))

		// First two chunks should be exactly STREAM_CHUNK_SIZE
		require.Len(t, chunks[0], STREAM_CHUNK_SIZE)
		require.Len(t, chunks[1], STREAM_CHUNK_SIZE)
	})

	t.Run("utf8 multi-byte characters at chunk boundaries", func(t *testing.T) {
		// Create a string with multi-byte UTF-8 characters
		// "你好" (ni hao) is 6 bytes (3 bytes per character)
		multiBytePrefix := strings.Repeat("你好", 1000)
		// Pad with ASCII to get close to the chunk boundary
		paddingSize := STREAM_CHUNK_SIZE - (len(multiBytePrefix) % STREAM_CHUNK_SIZE) - 2
		if paddingSize < 0 {
			paddingSize += STREAM_CHUNK_SIZE
		}
		padding := strings.Repeat("a", paddingSize)

		// Add a multi-byte character at the end to test boundary handling
		multiByteSuffix := "您"
		testString := multiBytePrefix + padding + multiByteSuffix

		chunks := chunkUtf8String(testString)

		require.Len(t, chunks, 2)

		// Validate each chunk is valid UTF-8
		for i, chunk := range chunks {
			require.True(t, utf8.Valid(chunk))

			switch i {
			case 0:
				require.Equal(t, len(multiBytePrefix+padding), len(chunk))
			case 1:
				require.Equal(t, len(multiByteSuffix), len(chunk))
			}
		}

		// Reconstruct and verify the original string
		var reconstructed []byte
		for _, chunk := range chunks {
			reconstructed = append(reconstructed, chunk...)
		}

		require.Equal(t, testString, string(reconstructed))
	})

	t.Run("string with various UTF-8 characters", func(t *testing.T) {
		// Mix of 1, 2, 3, and 4 byte UTF-8 characters
		// With a pattern that doesn't divide evenly into the chunk size
		testChars := []string{
			"a", // 1 byte (ASCII)
			"é", // 2 bytes (Latin-1 Supplement)
			"你", // 3 bytes (CJK)
			"🚀", // 4 bytes (Emoji)
			"世", // 3 bytes (another CJK character)
		}

		// Create a large string by repeating these characters
		var builder strings.Builder
		for i := 0; i < STREAM_CHUNK_SIZE; i++ {
			builder.WriteString(testChars[i%len(testChars)])
		}
		testString := builder.String()

		chunks := chunkUtf8String(testString)

		// Total string size calculation:
		// - We add STREAM_CHUNK_SIZE characters to the string (15,000 characters)
		// - These characters follow a repeating pattern of 5 different characters
		// - Each 5-character pattern consists of:
		//   * "a"  - 1 byte
		//   * "é"  - 2 bytes
		//   * "你" - 3 bytes
		//   * "🚀" - 4 bytes
		//   * "世" - 3 bytes
		// - Total: 13 bytes per 5-character pattern
		// - We have 15,000 / 5 = 3,000 complete patterns
		// - 3,000 patterns × 13 bytes = 39,000 bytes total string size

		// Expected chunk sizes based on the algorithm behavior:
		// - Each full pattern is exactly 13 bytes (a + é + 你 + 🚀 + 世 = 1+2+3+4+3 = 13 bytes)
		// - Since 13 doesn't divide evenly into 15,000, chunks need to adjust to respect UTF-8 boundaries
		// - We can calculate the exact chunk sizes by examining how patterns fit into chunks:
		//
		// - Chunk 1 (14999 bytes): 1,153 complete patterns = 14,989 bytes
		//      Then we can accomodate additional 10 bytes before crossing the max chunk size (1 + 2 + 3 + 4)
		// - Chunk 2 (14998 bytes): Another 1,153 patterns = 14,989 bytes
		//      Then we can accomodate additional 9 bytes before crossing the max chunk size (3 + 1 + 2 + 3)
		// - Chunk 3 (9003 bytes): The remaining bytes
		//
		// - Total: 39,000 bytes (14,999 + 14,998 + 9003)
		expectedSizes := []int{14999, 14998, 9003}
		for i, expectedSize := range expectedSizes {
			require.Equal(t, expectedSize, len(chunks[i]))
		}

		require.Len(t, chunks, len(expectedSizes))

		// Verify each chunk is valid UTF-8
		for _, chunk := range chunks {
			require.True(t, utf8.Valid(chunk))
		}

		// Reconstructed string should match the original
		var reconstructed []byte
		for _, chunk := range chunks {
			reconstructed = append(reconstructed, chunk...)
		}

		require.Equal(t, testString, string(reconstructed))
	})

	t.Run("utf8 boundary detection", func(t *testing.T) {
		// Create a string where a multi-byte character crosses the STREAM_CHUNK_SIZE boundary
		// The Chinese character "好" takes 3 bytes
		// We want to position it so the STREAM_CHUNK_SIZE index falls on the last byte or middle of the character

		// Create a prefix that puts the start of the Chinese character right before the STREAM_CHUNK_SIZE boundary
		prefix := strings.Repeat("a", STREAM_CHUNK_SIZE-2)

		// Now the 3-byte character "好" will span positions:
		// STREAM_CHUNK_SIZE-2, STREAM_CHUNK_SIZE-1, and STREAM_CHUNK_SIZE
		testString := prefix + "好" + "additional content"

		chunks := chunkUtf8String(testString)

		// We expect the function to detect that the byte at position STREAM_CHUNK_SIZE is a continuation byte
		// and back up to the start of the character
		require.Len(t, chunks, 2)

		// The first chunk should end before the Chinese character
		require.Len(t, chunks[0], len(prefix))

		// The second chunk should start with the character that would have been split
		expectedSecondChunk := "好" + "additional content"
		require.Equal(t, expectedSecondChunk, string(chunks[1]))

		// Verify all chunks are valid UTF-8
		validateUtf8Chunks(t, chunks)

		// Verify the reconstructed string matches the original
		var reconstructed []byte
		for _, chunk := range chunks {
			reconstructed = append(reconstructed, chunk...)
		}
		require.Equal(t, testString, string(reconstructed))
	})

	// Add a test with 4-byte UTF-8 characters (emojis) at the boundary
	t.Run("utf8 boundary with 4-byte characters", func(t *testing.T) {
		// Create a string where a 4-byte character crosses the STREAM_CHUNK_SIZE boundary
		// Position the emoji so the STREAM_CHUNK_SIZE index falls on one of its continuation bytes

		// Create a prefix that puts the start of the emoji right before the STREAM_CHUNK_SIZE boundary
		prefix := strings.Repeat("a", STREAM_CHUNK_SIZE-3)

		// Now the 4-byte character "🚀" will span positions:
		// STREAM_CHUNK_SIZE-3, STREAM_CHUNK_SIZE-2, STREAM_CHUNK_SIZE-1, and STREAM_CHUNK_SIZE
		testString := prefix + "🚀" + "more content"

		chunks := chunkUtf8String(testString)

		// We expect the function to detect that the byte at position STREAM_CHUNK_SIZE is a continuation byte
		// and back up to the start of the character
		require.Len(t, chunks, 2)

		// The first chunk should end before the emoji
		require.Len(t, chunks[0], len(prefix))

		// The second chunk should start with the emoji that would have been split
		expectedSecondChunk := "🚀" + "more content"
		require.Equal(t, expectedSecondChunk, string(chunks[1]))

		// Verify all chunks are valid UTF-8
		validateUtf8Chunks(t, chunks)
	})

	// Test a pathological case where a UTF-8 sequence is exactly at the chunk boundary
	t.Run("utf8 sequence exactly at chunk boundary", func(t *testing.T) {
		// Create a string that's exactly STREAM_CHUNK_SIZE - 1
		prefix := strings.Repeat("a", STREAM_CHUNK_SIZE-1)

		// Add a 2-byte character to cross the boundary
		// "é" takes 2 bytes in UTF-8
		testString := prefix + "é" + strings.Repeat("a", STREAM_CHUNK_SIZE)

		chunks := chunkUtf8String(testString)

		require.Len(t, chunks, 3)

		expectedSizes := []int{STREAM_CHUNK_SIZE - 1, STREAM_CHUNK_SIZE, 2}
		for i, expectedSize := range expectedSizes {
			require.Len(t, chunks[i], expectedSize)
		}

		// Verify all chunks are valid UTF-8
		validateUtf8Chunks(t, chunks)

		// Verify the reconstructed string matches the original
		var reconstructed []byte
		for _, chunk := range chunks {
			reconstructed = append(reconstructed, chunk...)
		}
		require.Equal(t, testString, string(reconstructed))
	})
}

// Helper function to validate that chunks properly handle UTF-8 boundaries
func validateUtf8Chunks(t *testing.T, chunks [][]byte) {
	for _, chunk := range chunks {
		require.True(t, utf8.Valid(chunk))
	}
}

func TestChunkBytes(t *testing.T) {
	t.Run("empty byte slice", func(t *testing.T) {
		chunks := chunkBytes([]byte{})
		require.Zero(t, len(chunks))
	})

	t.Run("byte slice shorter than chunk size", func(t *testing.T) {
		testData := bytes.Repeat([]byte{1, 2, 3}, 1000)
		chunks := chunkBytes(testData)

		require.Len(t, chunks, 1)
		require.Equal(t, testData, chunks[0])
	})

	t.Run("byte slice exactly at chunk size", func(t *testing.T) {
		testData := bytes.Repeat([]byte{1}, STREAM_CHUNK_SIZE)
		chunks := chunkBytes(testData)

		require.Len(t, chunks, 1)
		require.Equal(t, testData, chunks[0])
	})

	t.Run("byte slice longer than chunk size", func(t *testing.T) {
		testData := bytes.Repeat([]byte{1, 2, 3, 4}, STREAM_CHUNK_SIZE)
		// Add some extra bytes
		testData = append(testData, []byte{5, 6, 7, 8, 9, 10}...)

		chunks := chunkBytes(testData)

		expectedChunks := (len(testData) + STREAM_CHUNK_SIZE - 1) / STREAM_CHUNK_SIZE
		require.Len(t, chunks, expectedChunks)

		// Reconstruct the original data and check
		var reconstructed []byte
		for _, chunk := range chunks {
			reconstructed = append(reconstructed, chunk...)
		}

		require.Equal(t, testData, reconstructed)

		// Check sizes of chunks
		for i, chunk := range chunks {
			if i < len(chunks)-1 {
				// All chunks except possibly the last should be exactly STREAM_CHUNK_SIZE
				require.Len(t, chunk, STREAM_CHUNK_SIZE)
			} else if i == len(chunks)-1 && len(testData)%STREAM_CHUNK_SIZE != 0 {
				// Last chunk should be the remainder
				expectedSize := len(testData) % STREAM_CHUNK_SIZE
				require.Len(t, chunk, expectedSize)
			}
		}
	})

	t.Run("multiple full chunks", func(t *testing.T) {
		// Create a test data exactly 2.5 times STREAM_CHUNK_SIZE
		fullChunks := 2
		extraBytes := STREAM_CHUNK_SIZE / 2

		testData := bytes.Repeat([]byte{42}, STREAM_CHUNK_SIZE*fullChunks+extraBytes)
		chunks := chunkBytes(testData)

		require.Len(t, chunks, fullChunks+1)

		// Verify each full chunk is exactly STREAM_CHUNK_SIZE
		for i := 0; i < fullChunks; i++ {
			require.Len(t, chunks[i], STREAM_CHUNK_SIZE)
		}

		// Verify the last partial chunk size
		require.Len(t, chunks[fullChunks], extraBytes)
	})
}
