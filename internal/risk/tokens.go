package risk

import (
	"os"
	"regexp"
	"strings"

	"github.com/xasdta/pilothouse/internal/api"
)

// osTempDir is $TMPDIR (macOS: a per-user /var/folders path); tests swap it.
var osTempDir = os.TempDir

// tokenRE matches credentials with a recognisable shape. It is one RE2
// alternation, so a scan is a single linear pass.
var tokenRE = regexp.MustCompile(`(?:` + strings.Join([]string{
	`\bgh[pousr]_[A-Za-z0-9]{36,}`,                                       // GitHub tokens
	`\bgithub_pat_[A-Za-z0-9_]{50,}`,                                     // GitHub fine-grained tokens
	`\bglpat-[A-Za-z0-9_-]{20,}`,                                         // GitLab
	`\bxox[abposr]-[A-Za-z0-9-]{10,}`,                                    // Slack
	`\bsk-ant-[A-Za-z0-9_-]{20,}`,                                        // Anthropic
	`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{40,}`,                  // OpenAI
	`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`,                                      // AWS access key ids
	`\bAIza[0-9A-Za-z_-]{35}`,                                            // Google API keys
	`\bya29\.[0-9A-Za-z_-]{20,}`,                                         // Google OAuth access tokens
	`\b(?:sk|rk)_live_[0-9A-Za-z]{20,}`,                                  // Stripe live keys
	`\bnpm_[A-Za-z0-9]{36}\b`,                                            // npm
	`\bpypi-[A-Za-z0-9_-]{50,}`,                                          // PyPI
	`\bSG\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}`,                       // SendGrid
	`\bhf_[A-Za-z0-9]{30,}`,                                              // Hugging Face
	`-----BEGIN (?:[A-Z]+ )*PRIVATE KEY-----`,                            // PEM private keys
	`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`, // JWTs
	`(?i:authorization:\s*(?:bearer|token|basic)\s+[A-Za-z0-9._~+/=-]{16,})`,
	`(?i:--(?:password|passwd|api-key|apikey|token|secret|client-secret)[= ]\s*[^\s$'"-][^\s'"]{5,})`,
}, "|") + `)`)

// tokens adds a secrets label when s contains something shaped like a
// credential. The label never quotes it.
func (f *finder) tokens(s, text string) {
	if len(s) > maxScan {
		s = s[:maxScan]
	}
	if s != "" && tokenRE.MatchString(s) {
		f.add(api.RiskSecrets, api.RiskHigh, text)
	}
}

// Redact replaces what looks like a credential in s with "[redacted]",
// for text that is stored and shown on devices (an agent's last words).
func Redact(s string) string {
	return tokenRE.ReplaceAllString(s, "[redacted]")
}
