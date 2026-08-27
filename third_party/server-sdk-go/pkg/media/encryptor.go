package media

import (
	"crypto/aes"
	"crypto/cipher"
	"sync"

	"github.com/livekit/media-sdk"
	lksdk "github.com/livekit/server-sdk-go/v2"

	pmedia "github.com/pion/webrtc/v4/pkg/media"
)

type Encryptor interface {
	EncryptSample(payload []byte) ([]byte, error)
}

type GCMEncryptor struct {
	cipherBlock cipher.Block
	kid         uint8

	mu sync.Mutex
}

func NewGCMEncryptor(key []byte, kid uint8) (*GCMEncryptor, error) {
	cipherBlock, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	return &GCMEncryptor{
		cipherBlock: cipherBlock,
		kid:         kid,
	}, nil
}

func (e *GCMEncryptor) UpdateKeyAndKid(key []byte, kid uint8) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	cipherBlock, err := aes.NewCipher(key)
	if err != nil {
		return err
	}

	e.cipherBlock = cipherBlock
	e.kid = kid
	return nil
}

func (e *GCMEncryptor) EncryptSample(payload []byte) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	return lksdk.EncryptGCMAudioSampleCustomCipher(payload, e.kid, e.cipherBlock)
}

type CustomEncryptor struct {
	encryptFunc func(payload []byte) ([]byte, error)
}

func NewCustomEncryptor(encryptFunc func(payload []byte) ([]byte, error)) *CustomEncryptor {
	return &CustomEncryptor{encryptFunc: encryptFunc}
}

func (e *CustomEncryptor) EncryptSample(payload []byte) ([]byte, error) {
	return e.encryptFunc(payload)
}

type encryptionHandler struct {
	writer     media.MediaSampleWriter
	encryptor  Encryptor
	sampleRate int
}

func newEncryptionHandler(writer media.MediaSampleWriter, encryptor Encryptor, sampleRate int) *encryptionHandler {
	return &encryptionHandler{writer: writer, encryptor: encryptor, sampleRate: sampleRate}
}

func (e *encryptionHandler) WriteSample(sample pmedia.Sample) error {
	encryptedSampleData, err := e.encryptor.EncryptSample(sample.Data)
	if err != nil {
		return err
	}

	sample.Data = encryptedSampleData
	return e.writer.WriteSample(sample)
}

func (e *encryptionHandler) SampleRate() int {
	return e.sampleRate
}
