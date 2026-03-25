package v1alpha1

import (
	"regexp"
	"strings"
	"testing"
)

// DatabaseNamePattern is the regex used in the kubebuilder validation marker
// for PostgresSpec.Database. Keep this in sync with the marker in postgres_types.go:
//
//	+kubebuilder:validation:Pattern=`^[a-zA-Z0-9_][a-zA-Z0-9_.\-$]*$`
const DatabaseNamePattern = `^[a-zA-Z0-9_][a-zA-Z0-9_.\-$]*$`

const DatabaseNameMaxLength = 63

func TestDatabaseNamePattern_ValidNames(t *testing.T) {
	re := regexp.MustCompile(DatabaseNamePattern)

	validNames := []struct {
		name  string
		value string
	}{
		{"simple alphanumeric", "mydb"},
		{"with hyphens", "my-database"},
		{"with underscores", "my_database"},
		{"dotted name", "my.database"},
		{"multiple dots", "my.test.db"},
		{"leading digit", "123db"},
		{"all digits", "12345"},
		{"dollar sign", "test$db"},
		{"leading underscore", "_mydb"},
		{"mixed special chars", "1.my_db-test$x"},
		{"single char", "a"},
		{"single digit", "1"},
	}

	for _, tc := range validNames {
		t.Run(tc.name, func(t *testing.T) {
			if !re.MatchString(tc.value) {
				t.Errorf("expected %q to be accepted by pattern %s", tc.value, DatabaseNamePattern)
			}
		})
	}
}

func TestDatabaseNamePattern_InvalidNames(t *testing.T) {
	re := regexp.MustCompile(DatabaseNamePattern)

	invalidNames := []struct {
		name  string
		value string
	}{
		{"empty string", ""},
		{"contains space", "my database"},
		{"contains single quote", "my'db"},
		{"contains double quote", `my"db`},
		{"contains semicolon", "my;db"},
		{"leading dot", ".mydb"},
		{"leading hyphen", "-mydb"},
		{"leading dollar", "$mydb"},
		{"contains slash", "my/db"},
		{"contains backslash", `my\db`},
		{"contains null byte", "my\x00db"},
		{"contains equals", "my=db"},
		{"contains at sign", "my@db"},
		{"contains parenthesis", "my(db)"},
	}

	for _, tc := range invalidNames {
		t.Run(tc.name, func(t *testing.T) {
			if re.MatchString(tc.value) {
				t.Errorf("expected %q to be rejected by pattern %s", tc.value, DatabaseNamePattern)
			}
		})
	}
}

func TestDatabaseNameMaxLength(t *testing.T) {
	re := regexp.MustCompile(DatabaseNamePattern)

	t.Run("exactly 63 characters is valid", func(t *testing.T) {
		name := strings.Repeat("a", DatabaseNameMaxLength)
		if !re.MatchString(name) {
			t.Errorf("63-char name should match pattern")
		}
		if len(name) > DatabaseNameMaxLength {
			t.Errorf("expected length <= %d, got %d", DatabaseNameMaxLength, len(name))
		}
	})

	t.Run("64 characters exceeds max length", func(t *testing.T) {
		name := strings.Repeat("a", DatabaseNameMaxLength+1)
		if len(name) <= DatabaseNameMaxLength {
			t.Errorf("expected length > %d, got %d", DatabaseNameMaxLength, len(name))
		}
	})
}
