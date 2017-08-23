package beater

import (
	"bytes"
	"crypto/tls"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kabukky/httpscerts"
	"github.com/stretchr/testify/assert"

	"github.com/elastic/apm-server/processor/transaction"
	"github.com/elastic/apm-server/tests"
	"github.com/elastic/beats/libbeat/beat"
)

var tmpCertPath string

func TestMain(m *testing.M) {
	current, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	tmpCertPath = filepath.Join(current, "test_certs")
	os.Mkdir(tmpCertPath, os.ModePerm)

	code := m.Run()
	if code == 0 {
		os.RemoveAll(tmpCertPath)
	}
	os.Exit(code)
}

func setupHTTP(t *testing.T) (*http.Server, *http.Request) {
	if testing.Short() {
		t.Skip("skipping server test")
	}

	host := randomAddr()
	cfg := defaultConfig
	cfg.Host = host
	apm := newServer(cfg, func(_ []beat.Event) {})
	go run(apm, cfg.SSL)
	waitForServer(false, host)
	data, _ := tests.LoadValidData("transaction")
	req, err := http.NewRequest("POST", transaction.Endpoint, bytes.NewReader(data))
	assert.Nil(t, err)
	return apm, req
}

func setupHTTPS(t *testing.T, useCert bool, domain string) (*http.Server, string, []byte) {
	if testing.Short() {
		t.Skip("skipping server test")
	}

	host := randomAddr()
	cfg := defaultConfig
	cfg.Host = host
	apm := newServer(cfg, func(_ []beat.Event) {})
	if useCert {
		cert := path.Join(tmpCertPath, t.Name()+".crt")
		key := strings.Replace(cert, ".crt", ".key", -1)
		t.Log("generating certificate in ", cert)
		httpscerts.Generate(cert, key, domain)
		cfg.SSL = &SSLConfig{Cert: cert, PrivateKey: key}
	}

	t.Log("Starting server on ", host)
	go run(apm, cfg.SSL)

	waitForServer(true, host)

	data, _ := tests.LoadValidData("transaction")

	return apm, host, data
}

func randomAddr() string {
	l, _ := net.Listen("tcp", "localhost:0")
	l.Close()
	return l.Addr().String()
}

func insecureClient() *http.Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	return &http.Client{Transport: tr}
}

func waitForServer(secure bool, host string) {
	var check = func() int {
		var res *http.Response
		var err error
		if secure {
			res, err = insecureClient().Get("https://" + host + "/healthcheck")
		} else {
			res, err = http.Get("http://" + host + "/healthcheck")
		}

		if err != nil {
			return 500
		}
		return res.StatusCode
	}

	for i := 0; i <= 1000; i++ {
		time.Sleep(time.Second / 50)
		if check() == 200 {
			return
		}
	}
	panic("server run timeout (10 seconds)")
}

func TestDecode(t *testing.T) {
	transactionBytes, err := tests.LoadValidData("transaction")
	assert.Nil(t, err)
	buffer := bytes.NewReader(transactionBytes)

	req, err := http.NewRequest("POST", "_", buffer)
	req.Header.Add("Content-Type", "application/json")
	assert.Nil(t, err)

	res, err := decodeData(req)
	assert.Nil(t, err)
	assert.NotNil(t, res)

	body, err := ioutil.ReadAll(res)
	assert.Nil(t, err)
	assert.Equal(t, transactionBytes, body)
}

func TestServerOk(t *testing.T) {
	apm, req := setupHTTP(t)
	req.Header.Add("Content-Type", "application/json")
	defer stop(apm)

	rr := httptest.NewRecorder()
	apm.Handler.ServeHTTP(rr, req)
	assert.Equal(t, 202, rr.Code, rr.Body.String())
}

func TestServerNoContentType(t *testing.T) {
	apm, req := setupHTTP(t)
	defer stop(apm)

	rr := httptest.NewRecorder()
	apm.Handler.ServeHTTP(rr, req)
	assert.Equal(t, 400, rr.Code, rr.Body.String())
}

func TestServerSecureUnknownCA(t *testing.T) {

	apm, host, data := setupHTTPS(t, true, "127.0.0.1")
	defer stop(apm)

	_, err := http.Post("https://"+host+transaction.Endpoint, "application/json", bytes.NewReader(data))

	assert.Contains(t, err.Error(), "x509: certificate signed by unknown authority")
}

func TestServerSecureSkipVerify(t *testing.T) {

	apm, host, data := setupHTTPS(t, true, "127.0.0.1")
	defer stop(apm)

	res, err := insecureClient().Post("https://"+host+transaction.Endpoint, "application/json", bytes.NewReader(data))

	assert.Nil(t, err)
	assert.Equal(t, res.StatusCode, 202)
}

func TestServerSecureBadDomain(t *testing.T) {

	apm, host, data := setupHTTPS(t, true, "ELASTIC")
	defer stop(apm)

	_, err := http.Post("https://"+host+transaction.Endpoint, "application/json", bytes.NewReader(data))

	msgs := []string{
		"x509: certificate signed by unknown authority",
		"x509: cannot validate certificate for 127.0.0.1",
	}
	checkErrMsg := strings.Contains(err.Error(), msgs[0]) || strings.Contains(err.Error(), msgs[1])
	assert.True(t, checkErrMsg, err.Error())
}

func TestServerSecureBadIP(t *testing.T) {

	apm, host, data := setupHTTPS(t, true, "192.168.10.11")
	defer stop(apm)

	_, err := http.Post("https://"+host+transaction.Endpoint, "application/json", bytes.NewReader(data))

	msgs := []string{
		"x509: certificate signed by unknown authority",
		"x509: certificate is valid for 192.168.10.11, not 127.0.0.1",
	}
	checkErrMsg := strings.Contains(err.Error(), msgs[0]) || strings.Contains(err.Error(), msgs[1])
	assert.True(t, checkErrMsg, err.Error())
}

func TestServerBadProtocol(t *testing.T) {

	apm, host, data := setupHTTPS(t, true, "localhost")
	defer stop(apm)

	_, err := http.Post("http://"+host+transaction.Endpoint, "application/json", bytes.NewReader(data))

	assert.Contains(t, err.Error(), "malformed HTTP response")
}

func TestSSLEnabled(t *testing.T) {
	truthy := true
	falsy := false

	cases := [][]interface{}{
		{Config{}, false},
		{Config{SSL: &SSLConfig{Enabled: &truthy}}, true},
		{Config{SSL: &SSLConfig{Enabled: &falsy}}, false},
		{Config{SSL: &SSLConfig{Cert: "Cert"}}, true},
		{Config{SSL: &SSLConfig{Cert: "Cert", PrivateKey: "key"}}, true},
		{Config{SSL: &SSLConfig{Cert: "Cert", PrivateKey: "key", Enabled: &falsy}}, false},
	}

	for idx, testCase := range cases {
		config := testCase[0].(Config)
		expected := testCase[1].(bool)
		assert.Equal(t, expected, config.SSL.isEnabled(), "Test Case %d should be %t", idx, expected)
	}
}

func TestJSONFailureResponse(t *testing.T) {
	req, err := http.NewRequest("POST", "/transactions", nil)
	assert.Nil(t, err)
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()

	sendError(w, req, 400, "Cannot compare apples to oranges", false)

	resp := w.Result()
	body, _ := ioutil.ReadAll(resp.Body)
	assert.Equal(t, 400, w.Code)
	assert.Equal(t, body, []byte(`{"error":"Cannot compare apples to oranges"}`))
}

func TestJSONFailureResponseWhenAcceptingAnything(t *testing.T) {
	req, err := http.NewRequest("POST", "/transactions", nil)
	assert.Nil(t, err)
	req.Header.Set("Accept", "*/*")
	w := httptest.NewRecorder()

	sendError(w, req, 400, "Cannot compare apples to oranges", false)

	resp := w.Result()
	body, _ := ioutil.ReadAll(resp.Body)
	assert.Equal(t, 400, w.Code)
	assert.Equal(t, body, []byte(`{"error":"Cannot compare apples to oranges"}`))
}

func TestHTMLFailureResponse(t *testing.T) {
	req, err := http.NewRequest("POST", "/transactions", nil)
	assert.Nil(t, err)
	req.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()

	sendError(w, req, 400, "Cannot compare apples to oranges", false)

	resp := w.Result()
	body, _ := ioutil.ReadAll(resp.Body)
	assert.Equal(t, 400, w.Code)
	assert.Equal(t, body, []byte(`Cannot compare apples to oranges`))
}

func TestFailureResponseNoAcceptHeader(t *testing.T) {
	req, err := http.NewRequest("POST", "/transactions", nil)
	assert.Nil(t, err)

	req.Header.Del("Accept")

	w := httptest.NewRecorder()
	sendError(w, req, 400, "Cannot compare apples to oranges", false)

	resp := w.Result()
	body, _ := ioutil.ReadAll(resp.Body)
	assert.Equal(t, 400, w.Code)
	assert.Equal(t, body, []byte(`Cannot compare apples to oranges`))
}
