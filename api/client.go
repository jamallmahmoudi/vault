package api

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/hashicorp/errwrap"
	cleanhttp "github.com/hashicorp/go-cleanhttp"
	retryablehttp "github.com/hashicorp/go-retryablehttp"
	rootcerts "github.com/hashicorp/go-rootcerts"
	"github.com/hashicorp/vault/helper/consts"
	"github.com/hashicorp/vault/helper/dhutil"
	"github.com/hashicorp/vault/helper/jsonutil"
	"github.com/hashicorp/vault/helper/parseutil"
	"golang.org/x/net/http2"
	"golang.org/x/time/rate"
)

const EnvVaultAddress = "VAULT_ADDR"
const EnvVaultAgentAddr = "VAULT_AGENT_ADDR"
const EnvVaultCACert = "VAULT_CACERT"
const EnvVaultCAPath = "VAULT_CAPATH"
const EnvVaultClientCert = "VAULT_CLIENT_CERT"
const EnvVaultClientKey = "VAULT_CLIENT_KEY"
const EnvVaultClientTimeout = "VAULT_CLIENT_TIMEOUT"
const EnvVaultSkipVerify = "VAULT_SKIP_VERIFY"
const EnvVaultNamespace = "VAULT_NAMESPACE"
const EnvVaultTLSServerName = "VAULT_TLS_SERVER_NAME"
const EnvVaultWrapTTL = "VAULT_WRAP_TTL"
const EnvVaultMaxRetries = "VAULT_MAX_RETRIES"
const EnvVaultToken = "VAULT_TOKEN"
const EnvVaultMFA = "VAULT_MFA"
const EnvRateLimit = "VAULT_RATE_LIMIT"
const EnvTokenFileSinkPath = "VAULT_TOKEN_FILE_SINK_PATH"
const EnvAgentSinkName = "VAULT_AGENT_SINK_NAME"

// WrappingLookupFunc is a function that, given an HTTP verb and a path,
// returns an optional string duration to be used for response wrapping (e.g.
// "15s", or simply "15"). The path will not begin with "/v1/" or "v1/" or "/",
// however, end-of-path forward slashes are not trimmed, so must match your
// called path precisely.
type WrappingLookupFunc func(operation, path string) string

// Config is used to configure the creation of the client.
type Config struct {
	modifyLock sync.RWMutex

	// Address is the address of the Vault server. This should be a complete
	// URL such as "http://vault.example.com". If you need a custom SSL
	// cert or want to enable insecure mode, you need to specify a custom
	// HttpClient.
	Address string

	// AgentAddress is the address of the local Vault agent. This should be a
	// complete URL such as "http://vault.example.com".
	AgentAddress string

	// HttpClient is the HTTP client to use. Vault sets sane defaults for the
	// http.Client and its associated http.Transport created in DefaultConfig.
	// If you must modify Vault's defaults, it is suggested that you start with
	// that client and modify as needed rather than start with an empty client
	// (or http.DefaultClient).
	HttpClient *http.Client

	// MaxRetries controls the maximum number of times to retry when a 5xx
	// error occurs. Set to 0 to disable retrying. Defaults to 2 (for a total
	// of three tries).
	MaxRetries int

	// Timeout is for setting custom timeout parameter in the HttpClient
	Timeout time.Duration

	// If there is an error when creating the configuration, this will be the
	// error
	Error error

	// The Backoff function to use; a default is used if not provided
	Backoff retryablehttp.Backoff

	// Limiter is the rate limiter used by the client.
	// If this pointer is nil, then there will be no limit set.
	// In contrast, if this pointer is set, even to an empty struct,
	// then that limiter will be used. Note that an empty Limiter
	// is equivalent blocking all events.
	Limiter *rate.Limiter

	// OutputCurlString causes the actual request to return an error of type
	// *OutputStringError. Type asserting the error message will allow
	// fetching a cURL-compatible string for the operation.
	//
	// Note: It is not thread-safe to set this and make concurrent requests
	// with the same client. Cloning a client will not clone this value.
	OutputCurlString bool

	// TokenFileSinkPath specified a file to poll for vault tokens
	TokenFileSinkPath string

	// AgentSinkName names a Sink written to by an Agent and is needed when an Agent is writing to multiple sinks
	AgentSinkName string

	PollingInterval time.Duration
}

// TLSConfig contains the parameters needed to configure TLS on the HTTP client
// used to communicate with Vault.
type TLSConfig struct {
	// CACert is the path to a PEM-encoded CA cert file to use to verify the
	// Vault server SSL certificate.
	CACert string

	// CAPath is the path to a directory of PEM-encoded CA cert files to verify
	// the Vault server SSL certificate.
	CAPath string

	// ClientCert is the path to the certificate for Vault communication
	ClientCert string

	// ClientKey is the path to the private key for Vault communication
	ClientKey string

	// TLSServerName, if set, is used to set the SNI host when connecting via
	// TLS.
	TLSServerName string

	// Insecure enables or disables SSL verification
	Insecure bool
}

// DefaultConfig returns a default configuration for the client. It is
// safe to modify the return value of this function.
//
// The default Address is https://127.0.0.1:8200, but this can be overridden by
// setting the `VAULT_ADDR` environment variable.
//
// If an error is encountered, this will return nil.
func DefaultConfig() *Config {
	config := &Config{
		Address:    "https://127.0.0.1:8200",
		HttpClient: cleanhttp.DefaultPooledClient(),
	}
	config.HttpClient.Timeout = time.Second * 60
	config.PollingInterval = 61 * time.Second

	transport := config.HttpClient.Transport.(*http.Transport)
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	if err := http2.ConfigureTransport(transport); err != nil {
		config.Error = err
		return config
	}

	if err := config.ReadEnvironment(); err != nil {
		config.Error = err
		return config
	}

	// Ensure redirects are not automatically followed
	// Note that this is sane for the API client as it has its own
	// redirect handling logic (and thus also for command/meta),
	// but in e.g. http_test actual redirect handling is necessary
	config.HttpClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		// Returning this value causes the Go net library to not close the
		// response body and to nil out the error. Otherwise retry clients may
		// try three times on every redirect because it sees an error from this
		// function (to prevent redirects) passing through to it.
		return http.ErrUseLastResponse
	}

	config.Backoff = retryablehttp.LinearJitterBackoff
	config.MaxRetries = 2

	return config
}

// ConfigureTLS takes a set of TLS configurations and applies those to the the
// HTTP client.
func (c *Config) ConfigureTLS(t *TLSConfig) error {
	if c.HttpClient == nil {
		c.HttpClient = DefaultConfig().HttpClient
	}
	clientTLSConfig := c.HttpClient.Transport.(*http.Transport).TLSClientConfig

	var clientCert tls.Certificate
	foundClientCert := false

	switch {
	case t.ClientCert != "" && t.ClientKey != "":
		var err error
		clientCert, err = tls.LoadX509KeyPair(t.ClientCert, t.ClientKey)
		if err != nil {
			return err
		}
		foundClientCert = true
	case t.ClientCert != "" || t.ClientKey != "":
		return fmt.Errorf("both client cert and client key must be provided")
	}

	if t.CACert != "" || t.CAPath != "" {
		rootConfig := &rootcerts.Config{
			CAFile: t.CACert,
			CAPath: t.CAPath,
		}
		if err := rootcerts.ConfigureTLS(clientTLSConfig, rootConfig); err != nil {
			return err
		}
	}

	if t.Insecure {
		clientTLSConfig.InsecureSkipVerify = true
	}

	if foundClientCert {
		// We use this function to ignore the server's preferential list of
		// CAs, otherwise any CA used for the cert auth backend must be in the
		// server's CA pool
		clientTLSConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &clientCert, nil
		}
	}

	if t.TLSServerName != "" {
		clientTLSConfig.ServerName = t.TLSServerName
	}

	return nil
}

// ReadEnvironment reads configuration information from the environment. If
// there is an error, no configuration value is updated.
func (c *Config) ReadEnvironment() error {
	var envAddress string
	var envAgentAddress string
	var envCACert string
	var envCAPath string
	var envClientCert string
	var envClientKey string
	var envClientTimeout time.Duration
	var envInsecure bool
	var envTLSServerName string
	var envMaxRetries *uint64
	var envTokenFileSinkPath string
	var envAgentSinkName string
	var limit *rate.Limiter

	// Parse the environment variables
	if v := os.Getenv(EnvVaultAddress); v != "" {
		envAddress = v
	}
	if v := os.Getenv(EnvVaultAgentAddr); v != "" {
		envAgentAddress = v
	}
	if v := os.Getenv(EnvVaultMaxRetries); v != "" {
		maxRetries, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return err
		}
		envMaxRetries = &maxRetries
	}
	if v := os.Getenv(EnvVaultCACert); v != "" {
		envCACert = v
	}
	if v := os.Getenv(EnvVaultCAPath); v != "" {
		envCAPath = v
	}
	if v := os.Getenv(EnvVaultClientCert); v != "" {
		envClientCert = v
	}
	if v := os.Getenv(EnvVaultClientKey); v != "" {
		envClientKey = v
	}
	if v := os.Getenv(EnvRateLimit); v != "" {
		rateLimit, burstLimit, err := parseRateLimit(v)
		if err != nil {
			return err
		}
		limit = rate.NewLimiter(rate.Limit(rateLimit), burstLimit)
	}
	if t := os.Getenv(EnvVaultClientTimeout); t != "" {
		clientTimeout, err := parseutil.ParseDurationSecond(t)
		if err != nil {
			return fmt.Errorf("could not parse %q", EnvVaultClientTimeout)
		}
		envClientTimeout = clientTimeout
	}
	if v := os.Getenv(EnvVaultSkipVerify); v != "" {
		var err error
		envInsecure, err = strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("could not parse VAULT_SKIP_VERIFY")
		}
	}
	if v := os.Getenv(EnvTokenFileSinkPath); v != "" {
		envTokenFileSinkPath = v
	}
	if v := os.Getenv(EnvAgentSinkName); v != "" {
		envAgentSinkName = v
	}
	if v := os.Getenv(EnvVaultTLSServerName); v != "" {
		envTLSServerName = v
	}

	// Configure the HTTP clients TLS configuration.
	t := &TLSConfig{
		CACert:        envCACert,
		CAPath:        envCAPath,
		ClientCert:    envClientCert,
		ClientKey:     envClientKey,
		TLSServerName: envTLSServerName,
		Insecure:      envInsecure,
	}

	c.modifyLock.Lock()
	defer c.modifyLock.Unlock()

	c.Limiter = limit

	if err := c.ConfigureTLS(t); err != nil {
		return err
	}

	if envAddress != "" {
		c.Address = envAddress
	}

	if envAgentAddress != "" {
		c.AgentAddress = envAgentAddress
	}

	if envMaxRetries != nil {
		c.MaxRetries = int(*envMaxRetries)
	}

	if envClientTimeout != 0 {
		c.Timeout = envClientTimeout
	}

	if envTokenFileSinkPath != "" {
		c.TokenFileSinkPath = envTokenFileSinkPath
	}

	if envAgentSinkName != "" {
		c.AgentSinkName = envAgentSinkName
	}

	return nil
}

func parseRateLimit(val string) (rate float64, burst int, err error) {

	_, err = fmt.Sscanf(val, "%f:%d", &rate, &burst)
	if err != nil {
		rate, err = strconv.ParseFloat(val, 64)
		if err != nil {
			err = fmt.Errorf("%v was provided but incorrectly formatted", EnvRateLimit)
		}
		burst = int(rate)
	}

	return rate, burst, err

}

// Client is the client to the Vault API. Create a client with NewClient.
type Client struct {
	modifyLock         sync.RWMutex
	addr               *url.URL
	config             *Config
	token              string
	headers            http.Header
	wrappingLookupFunc WrappingLookupFunc
	mfaCreds           []string
	policyOverride     bool

	// whether or not a routine has been kicked off
	sinkPollingStarted bool
	// whether or not the value in the sink should clobber the client's current token
	useFileSinkForToken bool
	privateKey          []byte
	publicKey           []byte
	remotePublicKey     []byte
	sharedKey           []byte
}

// NewClient returns a new client for the given configuration.
//
// If the configuration is nil, Vault will use configuration from
// DefaultConfig(), which is the recommended starting configuration.
//
// If the environment variable `VAULT_TOKEN` is present, the token will be
// automatically added to the client. Otherwise, you must manually call
// `SetToken()`.
//
// If the environment variable `VAULT_TOKEN_FILE_SINK_PATH` is present and contains
// a valid filepath, that filepath will be used to set the client's token and
// continue to be polled and update the client's token
func NewClient(c *Config) (*Client, error) {
	def := DefaultConfig()
	if def == nil {
		return nil, fmt.Errorf("could not create/read default configuration")
	}
	if def.Error != nil {
		return nil, errwrap.Wrapf("error encountered setting up default configuration: {{err}}", def.Error)
	}

	var clientConfig *Config
	if c == nil {
		clientConfig = def
	} else {
		clientConfig = c.Clone()
	}

	// config values should come from clientConfig
	c = nil

	// fill in some required fields with default values if they haven't been set
	if clientConfig.HttpClient == nil {
		clientConfig.HttpClient = def.HttpClient
	}
	if clientConfig.HttpClient.Transport == nil {
		clientConfig.HttpClient.Transport = def.HttpClient.Transport
	}

	if clientConfig.PollingInterval == 0 {
		clientConfig.PollingInterval = def.PollingInterval
	}

	address := clientConfig.Address
	if clientConfig.AgentAddress != "" {
		address = clientConfig.AgentAddress
	}

	u, err := url.Parse(address)
	if err != nil {
		return nil, err
	}

	if strings.HasPrefix(address, "unix://") {
		socket := strings.TrimPrefix(address, "unix://")
		transport := clientConfig.HttpClient.Transport.(*http.Transport)
		transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
			return net.Dial("unix", socket)
		}

		// Since the address points to a unix domain socket, the scheme in the
		// *URL would be set to `unix`. The *URL in the client is expected to
		// be pointing to the protocol used in the application layer and not to
		// the transport layer. Hence, setting the fields accordingly.
		u.Scheme = "http"
		u.Host = socket
		u.Path = ""
	}

	client := &Client{
		addr:   u,
		config: clientConfig,
	}

	// determine how to get a token
	tokenSinkPath := ""
	switch {
	case os.Getenv(EnvVaultToken) != "":
		client.token = os.Getenv(EnvVaultToken)
	case client.config.TokenFileSinkPath != "":
		tokenSinkPath = client.config.TokenFileSinkPath
	case client.config.AgentAddress != "":
		tokenSinkPath, err = client.GetSinkPathFromAgent()
		if err != nil {
			return nil, errwrap.Wrapf(fmt.Sprintf("failed to determine tokenSinkPath given the provided agent address %q {{err}}", client.config.AgentAddress), err)
		}
		client.config.TokenFileSinkPath = tokenSinkPath
	default: // no token available yet
	}

	// start polling token from file sink if it is available
	if client.config.TokenFileSinkPath != "" {
		client.useFileSinkForToken = true
		//		token, err := client.readTokenFromFile()
		// if err != nil {
		// 	return nil, errwrap.Wrapf(fmt.Sprintf("failed to read token from file %q {{err}}", client.config.TokenFileSinkPath), err)
		// }
		// client.token = token
		// poll file for updates
		client.pollFileForToken()
	}

	return client, nil
}

// updates a client's address and the address in it's config
func (c *Client) SetClientAddress(address string) error {
	u, err := url.Parse(address)
	if err != nil {
		return errwrap.Wrapf("Error updating client's address: {{err}}", err)
	}
	c.addr = u
	c.config.Address = address
	return nil
}

// updates the TokenFileSinkPath of a client's config
func (c *Client) SetClientConfigTokenFileSinkPath(path string) {
	c.config.TokenFileSinkPath = path
}

// contacts agent for a file sink path and initiates DHExchange if needed
func (c *Client) GetSinkPathFromAgent() (string, error) {
	if c.config.AgentAddress == "" {
		return "", errors.New("an agent address in this client's config must be set first")
	}
	uri, err := url.Parse(c.config.AgentAddress)
	if err != nil {
		return "", err
	}
	if uri.Path != "" {
		return "", errors.New("configured agent address url should not specify a path")
	}
	uri.Path = consts.AgentPathFileSinks
	if c.config.AgentSinkName != "" {
		v := url.Values{}
		v.Set("sinkName", c.config.AgentSinkName)
		uri.RawQuery = v.Encode()
	}

	response, err := c.config.HttpClient.Get(uri.String())
	if err != nil {
		return "", err
	}
	if response != nil {
		defer response.Body.Close()
	}

	responseBodyBytes, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return "", err
	}

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("received error code %d from agent: %s", response.StatusCode, string(responseBodyBytes))
	}

	agentSink := AgentSink{}
	if err := jsonutil.DecodeJSON(responseBodyBytes, &agentSink); err != nil {
		return "", err
	}

	// If the token written to the sink path is not encrypted then we are done
	if agentSink.DHPath == "" {
		return agentSink.TokenFilePath, nil
	}

	// otherwise DHExchange
	if err := c.InitiateDHExchange(agentSink.DHType, agentSink.DHPath); err != nil {
		return "", err
	}

	return agentSink.TokenFilePath, nil
}

// Initiates a DH exchange and resumes polling sink for token
// Can be called multiple times to update the secret shared between client and agent
func (c *Client) InitiateDHExchange(dhtype string, dhpath string) error {
	// Only curve25519 is supported for now
	if dhtype != "curve25519" {
		return fmt.Errorf("dh_type %q is not supported", dhtype)
	}

	// Generate encryption parameters
	pub, pri, err := dhutil.GeneratePublicPrivateKey()
	if err != nil {
		return errwrap.Wrapf("error generating pub/pri key pair for dh exchange: {{err}}", err)
	}

	c.publicKey = pub
	c.privateKey = pri

	// write the public key to dh_path
	publicKeyInfo := &dhutil.PublicKeyInfo{
		Curve25519PublicKey: pub,
	}

	keyData, err := jsonutil.EncodeJSON(publicKeyInfo)
	if err != nil {
		return errwrap.Wrapf("error JSONEncoding public key: {{err}}", err)
	}

	if err := ioutil.WriteFile(dhpath, keyData, 0600); err != nil {
		return errwrap.Wrapf(fmt.Sprintf("error writing public key to provided dh_path %q: {{err}}", dhpath), err)
	}

	// determine whether polling needs to be initiated
	c.useFileSinkForToken = true
	if !c.sinkPollingStarted {
		c.pollFileForToken()
	}

	return nil
}

// starts a go routine to poll the specified file for a token
func (c *Client) pollFileForToken() {
	if !c.sinkPollingStarted {
		go func() {
			for {
				time.Sleep(c.config.PollingInterval)
				if c.useFileSinkForToken && c.config.TokenFileSinkPath != "" {
					token, err := c.readTokenFromFile()
					// update the client's token if it has changed and there was no error reading the file
					if err == nil && token != c.token {
						c.modifyLock.Lock()
						c.token = token
						c.modifyLock.Unlock()
					}
				}
			}
		}()
		c.sinkPollingStarted = true
	}
}

func (c *Client) readTokenFromFile() (string, error) {
	var tokenString string
	// read from sink file
	val, err := ioutil.ReadFile(c.config.TokenFileSinkPath)
	if err != nil {
		return "", errwrap.Wrapf(fmt.Sprintf("error reading token from file sink %q: {{err}}", c.config.TokenFileSinkPath), err)
	}

	// val could be a raw token or a json structure if it was encrypted
	if len(c.publicKey) == 0 {
		// assume token is not encrypted
		tokenString = strings.TrimSpace(string(val))
	} else {
		// assume token is encrypted
		sinkEnvelope := new(dhutil.Envelope)
		if err := jsonutil.DecodeJSON(val, sinkEnvelope); err != nil {
			return "", errwrap.Wrapf(fmt.Sprintf("error decoding JSON from file sink %q: {{err}}", c.config.TokenFileSinkPath), err)
		}

		// generate shared key if it is not available
		if len(c.sharedKey) == 0 {
			c.remotePublicKey = sinkEnvelope.Curve25519PublicKey
			c.sharedKey, err = dhutil.GenerateSharedKey(c.privateKey, c.remotePublicKey)
			if err != nil {
				return "", errwrap.Wrapf("error generating shared key: {{err}}", err)
			}
		}

		// attempt to decrypt the token
		plainText, err := dhutil.DecryptAES(c.sharedKey, sinkEnvelope.EncryptedPayload, sinkEnvelope.Nonce, []byte("")) // todo add aad field to config
		if err != nil {
			return "", errwrap.Wrapf(fmt.Sprintf("error decrypting token from file sink %q: {{err}}", c.config.TokenFileSinkPath), err)
		}

		// todo handle case that the token is wrapped...
		tokenString = strings.TrimSpace(string(plainText))
	}

	return tokenString, nil
}

// Sets the address of Vault in the client. The format of address should be
// "<Scheme>://<Host>:<Port>". Setting this on a client will override the
// value of VAULT_ADDR environment variable.
func (c *Client) SetAddress(addr string) error {
	c.modifyLock.Lock()
	defer c.modifyLock.Unlock()

	parsedAddr, err := url.Parse(addr)
	if err != nil {
		return errwrap.Wrapf("failed to set address: {{err}}", err)
	}

	c.addr = parsedAddr
	return nil
}

// Address returns the Vault URL the client is configured to connect to
func (c *Client) Address() string {
	c.modifyLock.RLock()
	defer c.modifyLock.RUnlock()

	return c.addr.String()
}

// SetLimiter will set the rate limiter for this client.
// This method is thread-safe.
// rateLimit and burst are specified according to https://godoc.org/golang.org/x/time/rate#NewLimiter
func (c *Client) SetLimiter(rateLimit float64, burst int) {
	c.modifyLock.RLock()
	c.config.modifyLock.Lock()
	defer c.config.modifyLock.Unlock()
	c.modifyLock.RUnlock()

	c.config.Limiter = rate.NewLimiter(rate.Limit(rateLimit), burst)
}

// SetMaxRetries sets the number of retries that will be used in the case of certain errors
func (c *Client) SetMaxRetries(retries int) {
	c.modifyLock.RLock()
	c.config.modifyLock.Lock()
	defer c.config.modifyLock.Unlock()
	c.modifyLock.RUnlock()

	c.config.MaxRetries = retries
}

// SetClientTimeout sets the client request timeout
func (c *Client) SetClientTimeout(timeout time.Duration) {
	c.modifyLock.RLock()
	c.config.modifyLock.Lock()
	defer c.config.modifyLock.Unlock()
	c.modifyLock.RUnlock()

	c.config.Timeout = timeout
}

func (c *Client) OutputCurlString() bool {
	c.modifyLock.RLock()
	c.config.modifyLock.RLock()
	defer c.config.modifyLock.RUnlock()
	c.modifyLock.RUnlock()

	return c.config.OutputCurlString
}

func (c *Client) SetOutputCurlString(curl bool) {
	c.modifyLock.RLock()
	c.config.modifyLock.Lock()
	defer c.config.modifyLock.Unlock()
	c.modifyLock.RUnlock()

	c.config.OutputCurlString = curl
}

// CurrentWrappingLookupFunc sets a lookup function that returns desired wrap TTLs
// for a given operation and path
func (c *Client) CurrentWrappingLookupFunc() WrappingLookupFunc {
	c.modifyLock.RLock()
	defer c.modifyLock.RUnlock()

	return c.wrappingLookupFunc
}

// SetWrappingLookupFunc sets a lookup function that returns desired wrap TTLs
// for a given operation and path
func (c *Client) SetWrappingLookupFunc(lookupFunc WrappingLookupFunc) {
	c.modifyLock.Lock()
	defer c.modifyLock.Unlock()

	c.wrappingLookupFunc = lookupFunc
}

// SetMFACreds sets the MFA credentials supplied either via the environment
// variable or via the command line.
func (c *Client) SetMFACreds(creds []string) {
	c.modifyLock.Lock()
	defer c.modifyLock.Unlock()

	c.mfaCreds = creds
}

// SetNamespace sets the namespace supplied either via the environment
// variable or via the command line.
func (c *Client) SetNamespace(namespace string) {
	c.modifyLock.Lock()
	defer c.modifyLock.Unlock()

	if c.headers == nil {
		c.headers = make(http.Header)
	}

	c.headers.Set(consts.NamespaceHeaderName, namespace)
}

// Token returns the access token being used by this client. It will
// return the empty string if there is no token set.
func (c *Client) Token() string {
	c.modifyLock.RLock()
	defer c.modifyLock.RUnlock()

	return c.token
}

// SetToken sets the token directly. This won't perform any auth
// verification, it simply sets the token properly for future requests.
// setting the token to "" will return to polling a file sink if one is available
func (c *Client) SetToken(v string) {
	if v != "" {
		c.modifyLock.Lock()
		defer c.modifyLock.Unlock()
		c.useFileSinkForToken = false
		c.token = v
	} else {
		c.modifyLock.Lock()
		defer c.modifyLock.Unlock()
		c.token = ""
		c.useFileSinkForToken = true
	}
}

// ClearToken deletes the token if it is set and returns to polling if a
// tokenFileSinkPath was configured
func (c *Client) ClearToken() {
	c.SetToken("")
}

// Headers gets the current set of headers used for requests. This returns a
// copy; to modify it make modifications locally and use SetHeaders.
func (c *Client) Headers() http.Header {
	c.modifyLock.RLock()
	defer c.modifyLock.RUnlock()

	if c.headers == nil {
		return nil
	}

	ret := make(http.Header)
	for k, v := range c.headers {
		for _, val := range v {
			ret[k] = append(ret[k], val)
		}
	}

	return ret
}

// SetHeaders sets the headers to be used for future requests.
func (c *Client) SetHeaders(headers http.Header) {
	c.modifyLock.Lock()
	defer c.modifyLock.Unlock()

	c.headers = headers
}

// SetBackoff sets the backoff function to be used for future requests.
func (c *Client) SetBackoff(backoff retryablehttp.Backoff) {
	c.modifyLock.RLock()
	c.config.modifyLock.Lock()
	defer c.config.modifyLock.Unlock()
	c.modifyLock.RUnlock()

	c.config.Backoff = backoff
}

// Clone creates a new client with the same configuration. Note that the same
// underlying http.Client is used; modifying the client from more than one
// goroutine at once may not be safe, so modify the client as needed and then
// clone.
//
// Also, only the client's config is currently copied; this means items not in
// the api.Config struct, such as policy override and wrapping function
// behavior, must currently then be set as desired on the new client.
func (c *Client) Clone() (*Client, error) {
	c.modifyLock.RLock()
	c.config.modifyLock.RLock()
	config := c.config
	c.modifyLock.RUnlock()

	newConfig := &Config{
		Address:    config.Address,
		HttpClient: config.HttpClient,
		MaxRetries: config.MaxRetries,
		Timeout:    config.Timeout,
		Backoff:    config.Backoff,
		Limiter:    config.Limiter,
	}
	config.modifyLock.RUnlock()

	return NewClient(newConfig)
}

// config.Clone returns a clone of the config it is called on
func (c *Config) Clone() *Config {
	defer c.modifyLock.RUnlock()
	c.modifyLock.RLock()

	newConfig := &Config{
		Address:           c.Address,
		AgentAddress:      c.AgentAddress,
		HttpClient:        c.HttpClient,
		MaxRetries:        c.MaxRetries,
		Timeout:           c.Timeout,
		Backoff:           c.Backoff,
		OutputCurlString:  c.OutputCurlString,
		TokenFileSinkPath: c.TokenFileSinkPath,
		AgentSinkName:     c.AgentSinkName,
		PollingInterval:   c.PollingInterval,
	}

	// deep copy rate.Limiter if it is set
	var newLimiter *rate.Limiter
	if c.Limiter != nil &&
		c.Limiter.Limit() != 0 &&
		c.Limiter.Burst() != 0 {
		newLimiter = rate.NewLimiter(c.Limiter.Limit(), c.Limiter.Burst())
		newConfig.Limiter = newLimiter
	}

	// create a new HttpClient and copy fields they are available
	if c.HttpClient != nil {
		newHttpClient := &http.Client{
			Transport:     c.HttpClient.Transport,
			CheckRedirect: c.HttpClient.CheckRedirect,
			Timeout:       c.HttpClient.Timeout,
		}
		newConfig.HttpClient = newHttpClient
	}

	return newConfig
}

// SetPolicyOverride sets whether requests should be sent with the policy
// override flag to request overriding soft-mandatory Sentinel policies (both
// RGPs and EGPs)
func (c *Client) SetPolicyOverride(override bool) {
	c.modifyLock.Lock()
	defer c.modifyLock.Unlock()

	c.policyOverride = override
}

// NewRequest creates a new raw request object to query the Vault server
// configured for this client. This is an advanced method and generally
// doesn't need to be called externally.
func (c *Client) NewRequest(method, requestPath string) *Request {
	c.modifyLock.RLock()
	addr := c.addr
	token := c.token
	mfaCreds := c.mfaCreds
	wrappingLookupFunc := c.wrappingLookupFunc
	headers := c.headers
	policyOverride := c.policyOverride
	c.modifyLock.RUnlock()

	// if SRV records exist (see https://tools.ietf.org/html/draft-andrews-http-srv-02), lookup the SRV
	// record and take the highest match; this is not designed for high-availability, just discovery
	var host string = addr.Host
	if addr.Port() == "" {
		// Internet Draft specifies that the SRV record is ignored if a port is given
		_, addrs, err := net.LookupSRV("http", "tcp", addr.Hostname())
		if err == nil && len(addrs) > 0 {
			host = fmt.Sprintf("%s:%d", addrs[0].Target, addrs[0].Port)
		}
	}

	req := &Request{
		Method: method,
		URL: &url.URL{
			User:   addr.User,
			Scheme: addr.Scheme,
			Host:   host,
			Path:   path.Join(addr.Path, requestPath),
		},
		ClientToken: token,
		Params:      make(map[string][]string),
	}

	var lookupPath string
	switch {
	case strings.HasPrefix(requestPath, "/v1/"):
		lookupPath = strings.TrimPrefix(requestPath, "/v1/")
	case strings.HasPrefix(requestPath, "v1/"):
		lookupPath = strings.TrimPrefix(requestPath, "v1/")
	default:
		lookupPath = requestPath
	}

	req.MFAHeaderVals = mfaCreds

	if wrappingLookupFunc != nil {
		req.WrapTTL = wrappingLookupFunc(method, lookupPath)
	} else {
		req.WrapTTL = DefaultWrappingLookupFunc(method, lookupPath)
	}

	if headers != nil {
		req.Headers = headers
	}

	req.PolicyOverride = policyOverride

	return req
}

// RawRequest performs the raw request given. This request may be against
// a Vault server not configured with this client. This is an advanced operation
// that generally won't need to be called externally.
func (c *Client) RawRequest(r *Request) (*Response, error) {
	return c.RawRequestWithContext(context.Background(), r)
}

// RawRequestWithContext performs the raw request given. This request may be against
// a Vault server not configured with this client. This is an advanced operation
// that generally won't need to be called externally.
func (c *Client) RawRequestWithContext(ctx context.Context, r *Request) (*Response, error) {
	c.modifyLock.RLock()
	token := c.token

	c.config.modifyLock.RLock()
	limiter := c.config.Limiter
	maxRetries := c.config.MaxRetries
	backoff := c.config.Backoff
	httpClient := c.config.HttpClient
	timeout := c.config.Timeout
	outputCurlString := c.config.OutputCurlString
	c.config.modifyLock.RUnlock()

	c.modifyLock.RUnlock()

	if limiter != nil {
		limiter.Wait(ctx)
	}

	// Sanity check the token before potentially erroring from the API
	idx := strings.IndexFunc(token, func(c rune) bool {
		return !unicode.IsPrint(c)
	})
	if idx != -1 {
		return nil, fmt.Errorf("configured Vault token contains non-printable characters and cannot be used")
	}

	redirectCount := 0
START:
	req, err := r.toRetryableHTTP()
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, fmt.Errorf("nil request created")
	}

	if outputCurlString {
		LastOutputStringError = &OutputStringError{Request: req}
		return nil, LastOutputStringError
	}

	if timeout != 0 {
		ctx, _ = context.WithTimeout(ctx, timeout)
	}
	req.Request = req.Request.WithContext(ctx)

	if backoff == nil {
		backoff = retryablehttp.LinearJitterBackoff
	}

	client := &retryablehttp.Client{
		HTTPClient:   httpClient,
		RetryWaitMin: 1000 * time.Millisecond,
		RetryWaitMax: 1500 * time.Millisecond,
		RetryMax:     maxRetries,
		CheckRetry:   retryablehttp.DefaultRetryPolicy,
		Backoff:      backoff,
		ErrorHandler: retryablehttp.PassthroughErrorHandler,
	}

	var result *Response
	resp, err := client.Do(req)
	if resp != nil {
		result = &Response{Response: resp}
	}
	if err != nil {
		if strings.Contains(err.Error(), "tls: oversized") {
			err = errwrap.Wrapf(
				"{{err}}\n\n"+
					"This error usually means that the server is running with TLS disabled\n"+
					"but the client is configured to use TLS. Please either enable TLS\n"+
					"on the server or run the client with -address set to an address\n"+
					"that uses the http protocol:\n\n"+
					"    vault <command> -address http://<address>\n\n"+
					"You can also set the VAULT_ADDR environment variable:\n\n\n"+
					"    VAULT_ADDR=http://<address> vault <command>\n\n"+
					"where <address> is replaced by the actual address to the server.",
				err)
		}
		return result, err
	}

	// Check for a redirect, only allowing for a single redirect
	if (resp.StatusCode == 301 || resp.StatusCode == 302 || resp.StatusCode == 307) && redirectCount == 0 {
		// Parse the updated location
		respLoc, err := resp.Location()
		if err != nil {
			return result, err
		}

		// Ensure a protocol downgrade doesn't happen
		if req.URL.Scheme == "https" && respLoc.Scheme != "https" {
			return result, fmt.Errorf("redirect would cause protocol downgrade")
		}

		// Update the request
		r.URL = respLoc

		// Reset the request body if any
		if err := r.ResetJSONBody(); err != nil {
			return result, err
		}

		// Retry the request
		redirectCount++
		goto START
	}

	if err := result.Error(); err != nil {
		return result, err
	}

	return result, nil
}
