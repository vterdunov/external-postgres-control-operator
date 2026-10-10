//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

const (
	r4JDBCVersion = "42.7.10"
	r4JDBCDriver  = "jdbc"
	r4LibPQDriver = "libpq"
	r4JDBCDigest  = "cab1cd67cfa25c25de4348e532298028288a877ba01c77d1619fe45416193387"
	r4LibPQImage  = "public.ecr.aws/docker/library/postgres:18.6-bookworm"
)

// These consumers use actual client parsers, not Go's URL decoder. Credentials
// are sent only over stdin; child output is discarded even when a client fails.
type r4ApplicationClients struct {
	dir, libpq, jdbc, java string
}

func r4NewApplicationClients(ctx context.Context) *r4ApplicationClients {
	_, source, _, ok := runtime.Caller(0)
	ExpectWithOffset(1, ok).To(BeTrue())
	support := filepath.Join(filepath.Dir(source), "testdata", "connection")
	c := &r4ApplicationClients{dir: GinkgoT().TempDir()}
	c.libpq = filepath.Join(c.dir, "libpq-client")
	c.jdbc = filepath.Join(c.dir, "postgresql-"+r4JDBCVersion+".jar")
	c.java = filepath.Join(c.dir, "JdbcClient.java")
	java, err := os.ReadFile(filepath.Join(support, "JdbcClient.java"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, os.WriteFile(c.java, java, 0600)).To(Succeed())
	var jar []byte
	if cached := os.Getenv("EPCO_TEST_JDBC_JAR"); cached != "" {
		jar, err = os.ReadFile(cached)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	} else {
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet,
			"https://repo.maven.apache.org/maven2/org/postgresql/postgresql/"+r4JDBCVersion+
				"/postgresql-"+r4JDBCVersion+".jar", nil)
		ExpectWithOffset(1, requestErr).NotTo(HaveOccurred())
		response, requestErr := (&http.Client{Timeout: 30 * time.Second}).Do(request)
		ExpectWithOffset(1, requestErr).NotTo(HaveOccurred())
		ExpectWithOffset(1, response.StatusCode).To(Equal(http.StatusOK))
		jar, err = io.ReadAll(io.LimitReader(response.Body, 2<<20))
		ExpectWithOffset(1, response.Body.Close()).To(Succeed())
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}
	digest := sha256.Sum256(jar)
	ExpectWithOffset(1, hex.EncodeToString(digest[:])).To(Equal(r4JDBCDigest), "pinned pgJDBC artifact checksum")
	ExpectWithOffset(1, os.WriteFile(c.jdbc, jar, 0600)).To(Succeed())
	library := "/opt/homebrew/opt/libpq/lib/libpq.dylib"
	args := []string{"-std=c11", "-Wall", "-Wextra", "-Werror", "-o", c.libpq,
		filepath.Join(support, "libpq-client.c")}
	if runtime.GOOS == "linux" {
		// Link only against the pinned image's public libpq ABI, then execute
		// inside that same image. Its full dependency closure stays in-image.
		triplet := "x86_64-linux-gnu"
		if runtime.GOARCH == "arm64" {
			triplet = "aarch64-linux-gnu"
		}
		container, createErr := exec.CommandContext(ctx, "docker", "create", "--label",
			"epco.test.run="+postgres.RunID, "--entrypoint", "true", r4LibPQImage).Output()
		ExpectWithOffset(1, createErr).NotTo(HaveOccurred())
		id := strings.TrimSpace(string(container))
		library = filepath.Join(c.dir, "libpq.so.5")
		copyErr := exec.CommandContext(ctx, "docker", "cp",
			id+":/usr/lib/"+triplet+"/libpq.so.5.18", library).Run()
		removeErr := exec.CommandContext(ctx, "docker", "rm", id).Run()
		ExpectWithOffset(1, copyErr).NotTo(HaveOccurred())
		ExpectWithOffset(1, removeErr).NotTo(HaveOccurred())
		args = append(args, "-D_POSIX_C_SOURCE=200809L", "-Wl,--allow-shlib-undefined")
	}
	args = append(args, library)
	ExpectWithOffset(1, exec.CommandContext(ctx, "cc", args...).Run()).To(Succeed(), "compile suite libpq ABI consumer")
	return c
}
func r4ClientEnvironment() []string {
	var env []string
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if !strings.HasPrefix(key, "PG") && key != "JAVA_TOOL_OPTIONS" && key != "JDK_JAVA_OPTIONS" &&
			key != "_JAVA_OPTIONS" && key != "LD_LIBRARY_PATH" && key != "DYLD_LIBRARY_PATH" {
			env = append(env, value)
		}
	}
	return append(env, "PGCONNECT_TIMEOUT=2", "PGGSSENCMODE=disable", "PGSSLCERTMODE=disable",
		"PGPASSFILE=/epco-no-ambient-passfile", "LC_ALL=C.UTF-8")
}
func (c *r4ApplicationClients) consume(ctx context.Context, secret *corev1.Secret, driver, mode string) bool {
	key := "uri"
	if driver == r4JDBCDriver {
		key = "jdbc-uri"
	}
	values := []string{string(secret.Data[key]), string(secret.Data["host"]), string(secret.Data["port"]),
		string(secret.Data["dbname"]), string(secret.Data["username"]), string(secret.Data["password"]),
		string(secret.Data["sslmode"]), string(secret.Data["sslrootcert"])}
	if driver == r4JDBCDriver && values[6] == tlsRequire && len(secret.Data["ca.crt"]) > 0 {
		values[6] = "verify-ca"
	}
	command := exec.CommandContext(ctx, c.libpq, mode)
	if driver == r4JDBCDriver {
		command = exec.CommandContext(ctx, "java", "-Duser.home="+c.dir,
			"--source", "17", "-cp", c.jdbc, c.java, mode)
	} else if runtime.GOOS == "linux" {
		command = exec.CommandContext(ctx, "docker", "run", "--rm", "-i", "--network", "host", "--label",
			"epco.test.run="+postgres.RunID, "--mount", "type=bind,source="+c.dir+",target="+c.dir+",readonly",
			"--entrypoint", c.libpq, r4LibPQImage, mode)
	}
	command.Env = r4ClientEnvironment()
	command.Stdin = strings.NewReader(strings.Join(values, "\n") + "\n")
	return command.Run() == nil
}
func (c *r4ApplicationClients) publicCA(ctx context.Context, f *r4ConnectionFixture) string {
	path := filepath.Join(c.dir, "ca + roots", "ca.crt")
	ExpectWithOffset(1, os.MkdirAll(filepath.Dir(path), 0700)).To(Succeed())
	ca, err := os.ReadFile(f.Server.TLS.CA)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, os.WriteFile(path, ca, 0600)).To(Succeed())
	f.editDatabase(ctx, func(spec *epco.DatabaseSpec) { spec.ConnectionSecrets[0].SSLRootCertPath = path })
	return path
}
func (c *r4ApplicationClients) pgpass(ctx context.Context, secret *corev1.Secret, database, role string) bool {
	passfile := filepath.Join(c.dir, "application.pgpass")
	ExpectWithOffset(1, os.WriteFile(passfile, secret.Data["pgpass"], 0600)).To(Succeed())
	info, err := os.Stat(passfile)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, info.Mode().Perm()).To(Equal(os.FileMode(0600)))
	// The driver reads the emitted passfile; no alternative credential source is
	// supplied. Values use keyword parameters so URI userinfo is not involved.
	escape := strings.NewReplacer("\\", "\\\\", "'", "\\'")
	connection := fmt.Sprintf("host=%s port=%s dbname='%s' user='%s' sslmode=verify-full sslrootcert='%s' passfile='%s'",
		secret.Data["host"], secret.Data["port"], escape.Replace(database),
		escape.Replace(role), secret.Data["sslrootcert"], passfile)
	copy := secret.DeepCopy()
	copy.Data["uri"] = []byte(connection)
	copy.Data["dbname"], copy.Data["username"] = []byte(database), []byte(role)
	return c.consume(ctx, copy, r4LibPQDriver, "connect")
}
func r4SpecialPassword() []byte {
	return []byte("colon: back\\slash * slash/ percent% plus+ 日本語")
}
