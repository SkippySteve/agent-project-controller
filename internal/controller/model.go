package controller

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RejectRefreshMaterial refuses an operator model Secret that carries a refresh token.
// Access material may be mounted at /run/model. The controller never copies an owner refresh token into it.
func RejectRefreshMaterial(data map[string][]byte) error {
	for key := range data {
		folded := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
		if folded == "refreshtoken" {
			return errors.New("model access must not include a refresh token")
		}
		switch key {
		case "access-token", "chatgpt-account-id", "chatgpt-plan-type", "expires-at":
		default:
			return errors.New("model access contains an unreviewed field")
		}
	}
	token, account := string(data["access-token"]), string(data["chatgpt-account-id"])
	if token == "" || len(token) > 32768 || !bearerToken.MatchString(token) || account == "" || len(account) > 256 || strings.ContainsAny(account, "\r\n\t {}[]\"") {
		return errors.New("model access requires bounded access and account fields")
	}
	plan := string(data["chatgpt-plan-type"])
	if len(plan) > 256 || strings.ContainsAny(plan, "\r\n\x00") {
		return errors.New("invalid model access plan")
	}
	expires, err := strconv.ParseFloat(string(data["expires-at"]), 64)
	if err != nil || math.IsNaN(expires) || math.IsInf(expires, 0) || expires <= float64(time.Now().Unix()+60) {
		return errors.New("model access is expired or lacks a valid expiry")
	}
	return nil
}

var bearerToken = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]+$`)
