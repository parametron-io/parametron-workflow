package intent

import "strings"

// ValidOrganizationLogin validates a GitHub organization login without normalizing
// it. Deployment membership must still be checked against resolved identities.
func ValidOrganizationLogin(s string) bool {
	if len(s) == 0 || len(s) > 39 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return !strings.Contains(s, "--")
}
