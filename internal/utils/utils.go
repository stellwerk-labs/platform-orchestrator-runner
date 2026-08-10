package utils

import (
	"bytes"
	"encoding/base64"

	"filippo.io/age"
	"github.com/pkg/errors"
)

func EncryptBytes(logs []byte, recipient age.Recipient) (string, error) {
	var encryptedData = &bytes.Buffer{}
	w, err := age.Encrypt(encryptedData, recipient)
	if err != nil {
		return "", errors.Wrap(err, "failed to encrypt outputs with public key")
	}
	if _, err := w.Write(logs); err != nil {
		return "", errors.Wrap(err, "failed to write to encrypted file")
	}
	if err := w.Close(); err != nil {
		return "", errors.Wrap(err, "failed to close encrypted file")
	} else {
		return base64.StdEncoding.EncodeToString(encryptedData.Bytes()), nil
	}
}
