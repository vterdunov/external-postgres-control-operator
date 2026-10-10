package postgres

import (
	"bytes"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ApplicationConnection contains only the application identity and public TLS inputs.
type ApplicationConnection struct {
	Username, Password, Host, Database, SSLMode, SSLRootCertPath string
	Port                                                         int32
	CA                                                           []byte
}

// BuildConnectionSecretData renders the accepted application credential formats.
// Administrative configuration and client certificates are deliberately not inputs.
func BuildConnectionSecretData(input ApplicationConnection) (map[string][]byte, error) {
	for _, value := range []string{input.Username, input.Host, input.Database, input.SSLRootCertPath} {
		if !exportableText(value) {
			return nil, &Failure{InvalidConfiguration, "An application connection value cannot be represented safely"}
		}
	}
	if !exportableText(input.Password) {
		return nil, &Failure{InvalidSecret, "The application password cannot be represented by the connection formats"}
	}
	if input.Username == "" || input.Database == "" || strings.TrimSpace(input.Host) == "" || strings.HasPrefix(input.Host, "/") || input.Port < 0 || input.Port > 65535 {
		return nil, &Failure{InvalidConfiguration, "The application identity or endpoint is invalid"}
	}
	mode := input.SSLMode
	if mode == "" {
		mode = tlsVerifyFull
	}
	switch mode {
	case tlsDisable, tlsAllow, tlsPrefer, tlsRequire, tlsVerifyCA, tlsVerifyFull:
	default:
		return nil, &Failure{InvalidConfiguration, "The application TLS mode is invalid"}
	}
	port := input.Port
	if port == 0 {
		port = 5432
	}
	portText := strconv.FormatInt(int64(port), 10)
	path := ""
	if len(input.CA) > 0 {
		if _, err := certificatePool(input.CA); err != nil {
			return nil, err
		}
		path = input.SSLRootCertPath
		if path == "" {
			path = "/etc/postgresql/ca.crt"
		}
	}
	endpoint := net.JoinHostPort(input.Host, portText)
	base := url.URL{Scheme: "postgresql", Host: endpoint, Path: "/" + input.Database, RawPath: "/" + percentEncode(input.Database)}
	uri := base.Clone()
	uri.User = url.UserPassword(input.Username, input.Password)
	uri.RawQuery = "sslmode=" + percentEncode(mode)
	if path != "" {
		uri.RawQuery += "&sslrootcert=" + percentEncode(path)
	}
	jdbcMode := mode
	if mode == tlsRequire && path != "" {
		jdbcMode = tlsVerifyCA
	}
	jdbc := base.Clone()
	query := url.Values{"user": {input.Username}, "password": {input.Password}, "sslmode": {jdbcMode}}
	if path != "" {
		query.Set("sslrootcert", path)
	}
	jdbc.RawQuery = query.Encode()
	uriText, jdbcText := uri.String(), "jdbc:"+jdbc.String()
	fields := []string{input.Host, portText, input.Database, input.Username, input.Password}
	for i, field := range fields {
		field = strings.ReplaceAll(strings.ReplaceAll(field, "\\", "\\\\"), ":", "\\:")
		if i < 4 {
			field = strings.ReplaceAll(field, "*", "\\*")
		}
		fields[i] = field
	}
	data := map[string][]byte{
		"username": []byte(input.Username), "user": []byte(input.Username), "password": []byte(input.Password),
		"host": []byte(input.Host), "port": []byte(portText), "dbname": []byte(input.Database),
		"uri": []byte(uriText), "fqdn-uri": []byte(uriText), "jdbc-uri": []byte(jdbcText), "fqdn-jdbc-uri": []byte(jdbcText),
		"pgpass": []byte(strings.Join(fields, ":") + "\n"), "sslmode": []byte(mode),
	}
	if path != "" {
		data["ca.crt"] = bytes.Clone(input.CA)
		data["sslrootcert"] = []byte(path)
	}
	return data, nil
}

func exportableText(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func percentEncode(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}
