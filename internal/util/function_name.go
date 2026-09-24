package util

import (
	"regexp"
)

var functionNameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_.:-]`)

// SanitizeFunctionName ensures a function name matches the requirements for Gemini/Vertex AI.
// It replaces invalid characters with underscores, ensures it starts with a letter or underscore,
// and truncates it to 64 characters if necessary.
// Regex Rule: [^a-zA-Z0-9_.:-] replaced with _.
func SanitizeFunctionName(name string) string {
	if name == "" {
		return ""
	}

	sanitized := functionNameSanitizer.ReplaceAllString(name, "_")

	if len(sanitized) > 0 {
		first := sanitized[0]
		if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || first == '_') {

			if len(sanitized) >= 64 {
				sanitized = sanitized[:63]
			}
			sanitized = "_" + sanitized
		}
	} else {
		sanitized = "_"
	}

	if len(sanitized) > 64 {
		sanitized = sanitized[:64]
	}
	return sanitized
}
