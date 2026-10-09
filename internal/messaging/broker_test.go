package messaging

import (
	"encoding/json"
	"example.com/wagering/internal/domain"
	"testing"
	"time"
)

func TestMessageAuthenticationAndHashes(t *testing.T) {
	key := "local-provider-a-signing-key-00000001"
	money, _ := domain.Parse("25.00", "BRL")
	env := Requested{MessageID: "msg-1", Type: "WagerTransactionRequested", OccurredAt: time.Now().UTC(), Data: RequestedData{Request: domain.Request{ProviderID: "provider-a", ExternalID: "tx-1", PlayerID: domain.ID(), WalletID: domain.ID(), RoundID: "round", GameID: "game", Kind: domain.Bet, Money: money}, Key: "actual-key"}}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{"provider-a": key}
	in, err := Decode(string(raw), Sign(raw, key), keys)
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := env.Data.Request.Hash()
	actual, _ := in.Request.Hash()
	if expected != actual || in.Key != "actual-key" {
		t.Fatal("HTTP/SQS mismatch")
	}
	pretty, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	equivalent, err := Decode(string(pretty), Sign(pretty, key), keys)
	if err != nil || in.MessageHash != equivalent.MessageHash {
		t.Fatal("not canonical", err)
	}
	for _, sig := range []string{"", Sign(raw, "wrong-key"), "garbage"} {
		if _, err = Decode(string(raw), sig, keys); err == nil {
			t.Fatal("unauthorized signature")
		}
	}
	env.Data.ProviderID = "provider-b"
	raw, _ = json.Marshal(env)
	if _, err = Decode(string(raw), Sign(raw, key), keys); err == nil {
		t.Fatal("provider spoof")
	}
}
