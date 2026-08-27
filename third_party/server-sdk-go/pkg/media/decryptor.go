package media

import (
	"crypto/aes"
	"crypto/cipher"

	"github.com/livekit/media-sdk/rtp"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"go.uber.org/atomic"
)

type Decryptor interface {
	DecryptSample(payload []byte) ([]byte, error)
}

type GCMDecryptor struct {
	cipherBlock atomic.Value
	sifTrailer  []byte
}

func NewGCMDecryptor(key []byte, sifTrailer []byte) (*GCMDecryptor, error) {
	cipherBlock, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	d := &GCMDecryptor{sifTrailer: sifTrailer}
	d.cipherBlock.Store(cipherBlock)
	return d, nil
}

func (d *GCMDecryptor) UpdateKey(key []byte) error {
	cipherBlock, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	d.cipherBlock.Store(cipherBlock)
	return nil
}

func (d *GCMDecryptor) DecryptSample(payload []byte) ([]byte, error) {
	cipherBlock := d.cipherBlock.Load().(cipher.Block)
	return lksdk.DecryptGCMAudioSampleCustomCipher(payload, d.sifTrailer, cipherBlock)
}

type CustomDecryptor struct {
	decryptionFunc func(payload []byte, sifTrailer []byte) ([]byte, error)
	sifTrailer     []byte
}

func NewCustomDecryptor(decryptionFunc func(payload []byte, sifTrailer []byte) ([]byte, error), sifTrailer []byte) *CustomDecryptor {
	return &CustomDecryptor{decryptionFunc: decryptionFunc, sifTrailer: sifTrailer}
}

func (d *CustomDecryptor) DecryptSample(payload []byte) ([]byte, error) {
	return d.decryptionFunc(payload, d.sifTrailer)
}

type decryptionHandler struct {
	handler   rtp.Handler
	decryptor Decryptor
}

func newDecryptionHandler(h rtp.Handler, decryptor Decryptor) *decryptionHandler {
	return &decryptionHandler{
		handler:   h,
		decryptor: decryptor,
	}
}

func (d *decryptionHandler) HandleRTP(h *rtp.Header, payload []byte) error {
	sample, err := d.decryptor.DecryptSample(payload)
	if err != nil {
		return err
	}

	if sample == nil {
		// drop server injected frames
		return nil
	}
	return d.handler.HandleRTP(h, sample)
}

func (d *decryptionHandler) String() string {
	return "DecryptionHandler " + d.handler.String()
}
