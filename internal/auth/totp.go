package auth

import "github.com/pquerna/otp/totp"

// NewTOTPSecret generates a base32 recovery/seed secret for the customer.
func NewTOTPSecret() (string, error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      "GoShop",
		AccountName: "customer",
	})
	if err != nil {
		return "", err
	}
	return key.Secret(), nil
}

// ProvisionTOTP returns a (secret, otpauth URL) pair before activation.
func ProvisionTOTP(email string) (secret, otpauthURL string, err error) {
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "GoShop", AccountName: email})
	if err != nil {
		return "", "", err
	}
	return key.Secret(), key.URL(), nil
}

// VerifyTOTP validates a 6-digit code against the secret with a small window.
func VerifyTOTP(secret, code string) bool {
	return totp.Validate(code, secret)
}
