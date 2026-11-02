package config

type AccountPolicy struct {
	RegistrationEnabled          bool `yaml:"registration_enabled" json:"registrationEnabled"`
	RegistrationApprovalRequired bool `yaml:"registration_approval_required" json:"registrationApprovalRequired"`
	TOTPEnabled                  bool `yaml:"totp_enabled" json:"totpEnabled"`
}
