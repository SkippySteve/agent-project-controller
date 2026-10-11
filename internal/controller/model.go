package controller

import (
	"errors"
	"strings"
)

// RejectRefreshMaterial refuses an operator model Secret that carries a refresh token.
// Access material may be mounted at /run/model. The controller never copies an owner refresh token into it.
func RejectRefreshMaterial(data map[string][]byte) error {
	for key := range data {
		folded := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
		if folded == "refreshtoken" {
			return errors.New("model access must not include a refresh token")
		}
	}
	return nil
}
