package discord

import (
	"encoding/json"
	"errors"
	"testing"
)

var errUnitTestDecrypt = errors.New("unit test: decrypt failed")

func TestDecodeCredentials_RoundTrip(t *testing.T) {
	raw, _ := json.Marshal(installConfig{
		AppID:             "123456789012345678",
		BotUsername:       "MulticaBot",
		BotTokenEncrypted: "cGxhaW4tYm90LXRva2Vu", // base64("plain-bot-token")
	})

	creds, err := decodeCredentials(raw, nil)
	if err != nil {
		t.Fatalf("decodeCredentials: %v", err)
	}
	if creds.BotID != "123456789012345678" {
		t.Errorf("BotID = %q, want app id", creds.BotID)
	}
	if creds.BotUsername != "MulticaBot" {
		t.Errorf("BotUsername = %q, want MulticaBot", creds.BotUsername)
	}
	if creds.BotToken != "plain-bot-token" {
		t.Errorf("BotToken = %q, want decoded plaintext", creds.BotToken)
	}
}

func TestDecodeCredentials_EmptyConfig(t *testing.T) {
	if _, err := decodeCredentials(nil, nil); err == nil {
		t.Error("expected an error for empty config")
	}
}

func TestDecodeCredentials_InvalidJSON(t *testing.T) {
	if _, err := decodeCredentials(json.RawMessage(`{not json`), nil); err == nil {
		t.Error("expected an error for malformed installation config JSON")
	}
}

func TestDecodeCredentials_DecryptError(t *testing.T) {
	raw, _ := json.Marshal(installConfig{
		AppID:             "123",
		BotTokenEncrypted: "cGxhaW4=",
	})
	decrypt := func(ciphertext []byte) ([]byte, error) {
		return nil, errUnitTestDecrypt
	}
	if _, err := decodeCredentials(raw, decrypt); err == nil {
		t.Error("expected an error when the decrypter fails")
	}
}

func TestDecodePublicConfig(t *testing.T) {
	raw, _ := json.Marshal(installConfig{
		AppID:             "123456789012345678",
		BotUsername:       "MulticaBot",
		BotTokenEncrypted: "should-not-appear",
	})
	pub := DecodePublicConfig(raw)
	if pub.BotID != "123456789012345678" || pub.BotUsername != "MulticaBot" {
		t.Errorf("PublicConfig = %+v, want app id + username only", pub)
	}
}

func TestDecodePublicConfig_MalformedYieldsZeroValue(t *testing.T) {
	pub := DecodePublicConfig(json.RawMessage(`not json`))
	if pub != (PublicConfig{}) {
		t.Errorf("PublicConfig = %+v, want zero value for malformed input", pub)
	}
}
