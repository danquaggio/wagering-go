package bootstrap

import (
	"go.uber.org/fx"
	"testing"
)

func TestFxComposition(t *testing.T) {
	if err := fx.ValidateApp(Options()); err != nil {
		t.Fatal(err)
	}
}
