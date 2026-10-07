package utils

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestUtils(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "Command runner") }

var _ = Describe("Run", func() {
	It("preserves the working directory and caller environment", func() {
		before, err := os.Getwd()
		Expect(err).NotTo(HaveOccurred())
		dir, err := filepath.EvalSymlinks(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		cmd := exec.Command("sh", "-c", "printf '%s:%s' \"$EPCO_FIXTURE\" \"$PWD\"")
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "EPCO_FIXTURE=preserved"}
		output, err := Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(output).To(Equal("preserved:" + dir))
		after, err := os.Getwd()
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
	})
	It("redacts credentials from output and errors", func() {
		cmd := exec.Command("sh", "-c", "printf '%s' \"$PGPASSWORD postgres://alice:secret@localhost/db "+
			"password=secret Authorization: Bearer secret\"; exit 9")
		cmd.Env = append(os.Environ(), "PGPASSWORD=hidden-value")
		output, err := Run(cmd)
		Expect(err).To(HaveOccurred())
		for _, value := range []string{"hidden-value", "secret", "postgres://alice"} {
			Expect(output).NotTo(ContainSubstring(value))
			Expect(err.Error()).NotTo(ContainSubstring(value))
		}
	})
	It("redacts quoted DSNs and standalone credential options", func() {
		result := Redact("password='two words' postgresql://u:p@host/db?sslmode=require " +
			"Authorization: Bearer abc --password pass --token=xyz")
		for _, value := range []string{"two words", "postgresql://", "abc", "pass ", "xyz"} {
			Expect(strings.Contains(result, value)).To(BeFalse(), result)
		}
	})
	It("redacts standalone output of credentials supplied as command arguments", func() {
		cmd := exec.Command("sh", "-c", "printf '%s' \"$2\"; exit 8", "fixture", "--password", "argv-credential")
		output, err := Run(cmd)
		Expect(err).To(HaveOccurred())
		Expect(output).NotTo(ContainSubstring("argv-credential"))
		Expect(err.Error()).NotTo(ContainSubstring("argv-credential"))
	})
	It("redacts complete keyword PostgreSQL connection strings", func() {
		connection := "host=private-db port=5432 user=operator dbname=accounts " +
			"password='two words' sslmode=verify-full"
		output := Redact(connection)
		Expect(output).To(Equal("[REDACTED]"))
	})

})
