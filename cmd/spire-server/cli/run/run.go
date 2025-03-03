package run

import (
	"bytes"
	"context"
	"crypto/x509/pkix"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/hashicorp/hcl"
	"github.com/hashicorp/hcl/hcl/ast"
	"github.com/hashicorp/hcl/hcl/printer"
	"github.com/imdario/mergo"
	"github.com/mitchellh/cli"
	"github.com/sirupsen/logrus"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/spire/pkg/common/catalog"
	common_cli "github.com/spiffe/spire/pkg/common/cli"
	"github.com/spiffe/spire/pkg/common/health"
	"github.com/spiffe/spire/pkg/common/idutil"
	"github.com/spiffe/spire/pkg/common/log"
	"github.com/spiffe/spire/pkg/common/telemetry"
	"github.com/spiffe/spire/pkg/common/util"
	"github.com/spiffe/spire/pkg/server"
	"github.com/spiffe/spire/pkg/server/authpolicy"
	bundleClient "github.com/spiffe/spire/pkg/server/bundle/client"
	"github.com/spiffe/spire/pkg/server/ca"
	"github.com/spiffe/spire/pkg/server/endpoints/bundle"
	"github.com/spiffe/spire/pkg/server/plugin/keymanager"
)

const (
	commandName = "run"

	defaultConfigPath         = "conf/server/server.conf"
	defaultSocketPath         = "/tmp/spire-server/private/api.sock"
	defaultLogLevel           = "INFO"
	defaultBundleEndpointPort = 443
)

var (
	defaultCASubject = pkix.Name{
		Country:      []string{"US"},
		Organization: []string{"SPIFFE"},
	}

	defaultRateLimit = true
)

// Config contains all available configurables, arranged by section
type Config struct {
	Server       *serverConfig               `hcl:"server"`
	Plugins      *catalog.HCLPluginConfigMap `hcl:"plugins"`
	Telemetry    telemetry.FileConfig        `hcl:"telemetry"`
	HealthChecks health.Config               `hcl:"health_checks"`
	UnusedKeys   []string                    `hcl:",unusedKeys"`
}

type serverConfig struct {
	AuditLogEnabled bool               `hcl:"audit_log_enabled"`
	BindAddress     string             `hcl:"bind_address"`
	BindPort        int                `hcl:"bind_port"`
	CAKeyType       string             `hcl:"ca_key_type"`
	CASubject       *caSubjectConfig   `hcl:"ca_subject"`
	CATTL           string             `hcl:"ca_ttl"`
	DataDir         string             `hcl:"data_dir"`
	DefaultSVIDTTL  string             `hcl:"default_svid_ttl"`
	Experimental    experimentalConfig `hcl:"experimental"`
	Federation      *federationConfig  `hcl:"federation"`
	JWTIssuer       string             `hcl:"jwt_issuer"`
	JWTKeyType      string             `hcl:"jwt_key_type"`
	LogFile         string             `hcl:"log_file"`
	LogLevel        string             `hcl:"log_level"`
	LogFormat       string             `hcl:"log_format"`
	RateLimit       rateLimitConfig    `hcl:"ratelimit"`
	SocketPath      string             `hcl:"socket_path"`
	TrustDomain     string             `hcl:"trust_domain"`

	ConfigPath string
	ExpandEnv  bool

	// Undocumented configurables
	ProfilingEnabled bool     `hcl:"profiling_enabled"`
	ProfilingPort    int      `hcl:"profiling_port"`
	ProfilingFreq    int      `hcl:"profiling_freq"`
	ProfilingNames   []string `hcl:"profiling_names"`

	// TODO: Remove support for deprecated registration_uds_path in 1.1.0
	DeprecatedRegistrationUDSPath string `hcl:"registration_uds_path"`

	// TODO: Remove for 1.1.0
	AllowUnsafeIDs *bool `hcl:"allow_unsafe_ids"`

	UnusedKeys []string `hcl:",unusedKeys"`
}

type experimentalConfig struct {
	CacheReloadInterval string `hcl:"cache_reload_interval"`

	UnusedKeys []string `hcl:",unusedKeys"`

	AuthOpaPolicyEngine *authpolicy.OpaEngineConfig `hcl:"auth_opa_policy_engine"`
}

type caSubjectConfig struct {
	Country      []string `hcl:"country"`
	Organization []string `hcl:"organization"`
	CommonName   string   `hcl:"common_name"`
	UnusedKeys   []string `hcl:",unusedKeys"`
}

type federationConfig struct {
	BundleEndpoint *bundleEndpointConfig          `hcl:"bundle_endpoint"`
	FederatesWith  map[string]federatesWithConfig `hcl:"federates_with"`
	UnusedKeys     []string                       `hcl:",unusedKeys"`
}

type bundleEndpointConfig struct {
	Address    string                    `hcl:"address"`
	Port       int                       `hcl:"port"`
	ACME       *bundleEndpointACMEConfig `hcl:"acme"`
	UnusedKeys []string                  `hcl:",unusedKeys"`
}

type bundleEndpointACMEConfig struct {
	DirectoryURL string   `hcl:"directory_url"`
	DomainName   string   `hcl:"domain_name"`
	Email        string   `hcl:"email"`
	ToSAccepted  bool     `hcl:"tos_accepted"`
	UnusedKeys   []string `hcl:",unusedKeys"`
}

type federatesWithConfig struct {
	// TODO: Remove support for deprecated bundle_endpoint config in 1.1.0
	DeprecatedBundleEndpoint *deprecatedFederatesWithBundleEndpointConfig `hcl:"bundle_endpoint"`

	BundleEndpointURL     string   `hcl:"bundle_endpoint_url"`
	BundleEndpointProfile ast.Node `hcl:"bundle_endpoint_profile"`
	UnusedKeys            []string `hcl:",unusedKeys"`
}

type deprecatedFederatesWithBundleEndpointConfig struct {
	Address    string   `hcl:"address"`
	Port       int      `hcl:"port"`
	SpiffeID   string   `hcl:"spiffe_id"`
	UseWebPKI  bool     `hcl:"use_web_pki"`
	UnusedKeys []string `hcl:",unusedKeys"`
}

type bundleEndpointProfileConfig struct {
	HTTPSSPIFFE *httpsSPIFFEProfileConfig `hcl:"https_spiffe"`
	HTTPSWeb    *httpsWebProfileConfig    `hcl:"https_web"`
	UnusedKeys  []string                  `hcl:",unusedKeys"`
}

type httpsSPIFFEProfileConfig struct {
	EndpointSPIFFEID string   `hcl:"endpoint_spiffe_id"`
	UnusedKeys       []string `hcl:",unusedKeys"`
}

type httpsWebProfileConfig struct {
}

type rateLimitConfig struct {
	Attestation *bool    `hcl:"attestation"`
	Signing     *bool    `hcl:"signing"`
	UnusedKeys  []string `hcl:",unusedKeys"`
}

func NewRunCommand(logOptions []log.Option, allowUnknownConfig bool) cli.Command {
	return newRunCommand(common_cli.DefaultEnv, logOptions, allowUnknownConfig)
}

func newRunCommand(env *common_cli.Env, logOptions []log.Option, allowUnknownConfig bool) *Command {
	return &Command{
		env:                env,
		logOptions:         logOptions,
		allowUnknownConfig: allowUnknownConfig,
	}
}

// Run Command struct
type Command struct {
	logOptions         []log.Option
	env                *common_cli.Env
	allowUnknownConfig bool
}

// Help prints the server cmd usage
func (cmd *Command) Help() string {
	return Help(commandName, cmd.env.Stderr)
}

// Help is a standalone function that prints a help message to writer.
// It is used by both the run and validate commands, so they can share flag usage messages.
func Help(name string, writer io.Writer) string {
	_, err := parseFlags(name, []string{"-h"}, writer)
	// Error is always present because -h is passed
	return err.Error()
}

func LoadConfig(name string, args []string, logOptions []log.Option, output io.Writer, allowUnknownConfig bool) (*server.Config, error) {
	// First parse the CLI flags so we can get the config
	// file path, if set
	cliInput, err := parseFlags(name, args, output)
	if err != nil {
		return nil, err
	}

	// Load and parse the config file using either the default
	// path or CLI-specified value
	fileInput, err := ParseFile(cliInput.ConfigPath, cliInput.ExpandEnv)
	if err != nil {
		return nil, err
	}

	input, err := mergeInput(fileInput, cliInput)
	if err != nil {
		return nil, err
	}

	return NewServerConfig(input, logOptions, allowUnknownConfig)
}

// Run the SPIFFE Server
func (cmd *Command) Run(args []string) int {
	c, err := LoadConfig(commandName, args, cmd.logOptions, cmd.env.Stderr, cmd.allowUnknownConfig)
	if err != nil {
		_, _ = fmt.Fprintln(cmd.env.Stderr, err)
		return 1
	}

	// Set umask before starting up the server
	common_cli.SetUmask(c.Log)

	s := server.New(*c)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	util.SignalListener(ctx, cancel)

	err = s.Run(ctx)
	if err != nil {
		c.Log.WithError(err).Error("Server crashed")
		return 1
	}

	c.Log.Info("Server stopped gracefully")
	return 0
}

// Synopsis of the command
func (*Command) Synopsis() string {
	return "Runs the server"
}

func ParseFile(path string, expandEnv bool) (*Config, error) {
	c := &Config{}

	if path == "" {
		path = defaultConfigPath
	}

	// Return a friendly error if the file is missing
	byteData, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		absPath, err := filepath.Abs(path)
		if err != nil {
			msg := "could not determine CWD; config file not found at %s: use -config"
			return nil, fmt.Errorf(msg, path)
		}

		msg := "could not find config file %s: please use the -config flag"
		return nil, fmt.Errorf(msg, absPath)
	}
	if err != nil {
		return nil, fmt.Errorf("unable to read configuration at %q: %w", path, err)
	}
	data := string(byteData)

	// If envTemplate flag is passed, substitute $VARIABLES in configuration file
	if expandEnv {
		data = os.ExpandEnv(data)
	}

	if err := hcl.Decode(&c, data); err != nil {
		return nil, fmt.Errorf("unable to decode configuration at %q: %w", path, err)
	}

	return c, nil
}

func parseFlags(name string, args []string, output io.Writer) (*serverConfig, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(output)
	c := &serverConfig{}

	flags.StringVar(&c.BindAddress, "bindAddress", "", "IP address or DNS name of the SPIRE server")
	flags.IntVar(&c.BindPort, "serverPort", 0, "Port number of the SPIRE server")
	flags.StringVar(&c.ConfigPath, "config", "", "Path to a SPIRE config file")
	flags.StringVar(&c.DataDir, "dataDir", "", "Directory to store runtime data to")
	flags.StringVar(&c.LogFile, "logFile", "", "File to write logs to")
	flags.StringVar(&c.LogFormat, "logFormat", "", "'text' or 'json'")
	flags.StringVar(&c.LogLevel, "logLevel", "", "'debug', 'info', 'warn', or 'error'")
	flags.StringVar(&c.DeprecatedRegistrationUDSPath, "registrationUDSPath", "", "Path to bind the SPIRE Server API socket to (deprecated; use -socketPath)")
	// TODO: in 1.1.0. After registrationUDSPath is deprecated, we can put back the
	// default flag value on socketPath like it was previously, since we'll no
	// longer need to detect an unset flag from the default for deprecation
	// logging/error handling purposes.
	flags.StringVar(&c.SocketPath, "socketPath", "", `Path to bind the SPIRE Server API socket to (default "`+defaultSocketPath+`")`)
	flags.StringVar(&c.TrustDomain, "trustDomain", "", "The trust domain that this server belongs to")
	flags.BoolVar(&c.ExpandEnv, "expandEnv", false, "Expand environment variables in SPIRE config file")

	err := flags.Parse(args)
	if err != nil {
		return c, err
	}

	return c, nil
}

func mergeInput(fileInput *Config, cliInput *serverConfig) (*Config, error) {
	c := &Config{Server: &serverConfig{}}

	// Highest precedence first
	err := mergo.Merge(c.Server, cliInput)
	if err != nil {
		return nil, err
	}

	err = mergo.Merge(c, fileInput)
	if err != nil {
		return nil, err
	}

	err = mergo.Merge(c, defaultConfig())
	if err != nil {
		return nil, err
	}

	return c, nil
}

func NewServerConfig(c *Config, logOptions []log.Option, allowUnknownConfig bool) (*server.Config, error) {
	sc := &server.Config{}

	if err := validateConfig(c); err != nil {
		return nil, err
	}

	logOptions = append(logOptions,
		log.WithLevel(c.Server.LogLevel),
		log.WithFormat(c.Server.LogFormat),
		log.WithOutputFile(c.Server.LogFile))

	logger, err := log.NewLogger(logOptions...)
	if err != nil {
		return nil, fmt.Errorf("could not start logger: %w", err)
	}
	sc.Log = logger

	// This is a terrible hack but is just a short-term band-aid.
	// TODO: Deprecated and should be removed in 1.1
	if c.Server.AllowUnsafeIDs != nil {
		sc.Log.Warn("The insecure allow_unsafe_ids configurable is deprecated and will be removed in a future release.")
		idutil.SetAllowUnsafeIDs(*c.Server.AllowUnsafeIDs)
	}

	ip := net.ParseIP(c.Server.BindAddress)
	if ip == nil {
		return nil, fmt.Errorf("could not parse bind_address %q", c.Server.BindAddress)
	}
	sc.BindAddress = &net.TCPAddr{
		IP:   ip,
		Port: c.Server.BindPort,
	}

	var socketPath string
	switch {
	case c.Server.SocketPath != "":
		socketPath = c.Server.SocketPath
	case c.Server.DeprecatedRegistrationUDSPath != "":
		logger.Warn("The registration_uds_path configurable is deprecated; use socket_path instead.")
		socketPath = c.Server.DeprecatedRegistrationUDSPath
	default:
		socketPath = defaultSocketPath
	}

	sc.BindUDSAddress = &net.UnixAddr{
		Name: socketPath,
		Net:  "unix",
	}

	sc.DataDir = c.Server.DataDir
	sc.AuditLogEnabled = c.Server.AuditLogEnabled

	td, err := idutil.TrustDomainFromString(c.Server.TrustDomain)
	if err != nil {
		return nil, fmt.Errorf("could not parse trust_domain %q: %w", c.Server.TrustDomain, err)
	}
	common_cli.WarnOnLongTrustDomainName(td, logger)
	sc.TrustDomain = td

	if c.Server.RateLimit.Attestation == nil {
		c.Server.RateLimit.Attestation = &defaultRateLimit
	}
	sc.RateLimit.Attestation = *c.Server.RateLimit.Attestation

	if c.Server.RateLimit.Signing == nil {
		c.Server.RateLimit.Signing = &defaultRateLimit
	}
	sc.RateLimit.Signing = *c.Server.RateLimit.Signing

	if c.Server.Federation != nil {
		if c.Server.Federation.BundleEndpoint != nil {
			sc.Federation.BundleEndpoint = &bundle.EndpointConfig{
				Address: &net.TCPAddr{
					IP:   net.ParseIP(c.Server.Federation.BundleEndpoint.Address),
					Port: c.Server.Federation.BundleEndpoint.Port,
				},
			}

			if acme := c.Server.Federation.BundleEndpoint.ACME; acme != nil {
				sc.Federation.BundleEndpoint.ACME = &bundle.ACMEConfig{
					DirectoryURL: acme.DirectoryURL,
					DomainName:   acme.DomainName,
					CacheDir:     filepath.Join(sc.DataDir, "bundle-acme"),
					Email:        acme.Email,
					ToSAccepted:  acme.ToSAccepted,
				}
			}
		}

		federatesWith := map[spiffeid.TrustDomain]bundleClient.TrustDomainConfig{}

		for trustDomain, config := range c.Server.Federation.FederatesWith {
			td, err := idutil.TrustDomainFromString(trustDomain)
			if err != nil {
				return nil, err
			}

			var trustDomainConfig *bundleClient.TrustDomainConfig
			switch {
			case config.DeprecatedBundleEndpoint != nil && config.BundleEndpointProfile != nil:
				return nil, fmt.Errorf("error parsing federation relationship for trust domain %q: either the deprecated `bundle_endpoint` configuration section or the `bundle_endpoint_url` setting can be used, but not both", trustDomain)
			case config.DeprecatedBundleEndpoint != nil:
				sc.Log.Warn("The `bundle_endpoint` configurable inside `federates_with` is deprecated and will be removed in a future release. Please use `bundle_endpoint_url` and `bundle_endpoint_profile` to configure the federation with %q instead.", trustDomain)
				trustDomainConfig, err = parseDeprecatedBundleEndpoint(config.DeprecatedBundleEndpoint)
				if err != nil {
					return nil, fmt.Errorf("error parsing federation relationship for trust domain %q: %w", trustDomain, err)
				}
				if httpsSPIFFE, ok := trustDomainConfig.EndpointProfile.(bundleClient.HTTPSSPIFFEProfile); ok {
					if httpsSPIFFE.EndpointSPIFFEID.IsZero() {
						sc.Log.Warnf("federation.federates_with[\"%s\"].bundle_endpoint.spiffe_id is not specified in the SPIFFE Authentication configuration. A specific SPIFFE ID will be required in a future release in the https_spiffe profile.", trustDomain)
					}
				}
			case config.BundleEndpointProfile != nil:
				trustDomainConfig, err = parseBundleEndpointProfile(config)
				if err != nil {
					return nil, fmt.Errorf("error parsing federation relationship for trust domain %q: %w", trustDomain, err)
				}
			default:
				return nil, fmt.Errorf("federation configuration for trust domain %q: missing bundle endpoint configuration", trustDomain)
			}
			federatesWith[td] = *trustDomainConfig
		}
		sc.Federation.FederatesWith = federatesWith
	}

	sc.ProfilingEnabled = c.Server.ProfilingEnabled
	sc.ProfilingPort = c.Server.ProfilingPort
	sc.ProfilingFreq = c.Server.ProfilingFreq
	sc.ProfilingNames = c.Server.ProfilingNames

	if c.Server.DefaultSVIDTTL != "" {
		ttl, err := time.ParseDuration(c.Server.DefaultSVIDTTL)
		if err != nil {
			return nil, fmt.Errorf("could not parse default SVID ttl %q: %w", c.Server.DefaultSVIDTTL, err)
		}
		sc.SVIDTTL = ttl
	}

	if c.Server.CATTL != "" {
		ttl, err := time.ParseDuration(c.Server.CATTL)
		if err != nil {
			return nil, fmt.Errorf("could not parse default CA ttl %q: %w", c.Server.CATTL, err)
		}
		sc.CATTL = ttl
	}

	// If the configured TTLs can lead to surprises, then do our best to log an
	// accurate message and guide the user to resolution
	if !hasCompatibleTTLs(sc.CATTL, sc.SVIDTTL) {
		msgCATTLTooSmall := fmt.Sprintf(
			"The default_svid_ttl is too high for the configured ca_ttl value. "+
				"SVIDs with shorter lifetimes may be issued. "+
				"Please set the default_svid_ttl to %v or less, or the ca_ttl to %v or more, "+
				"to guarantee the full default_svid_ttl lifetime when CA rotations are scheduled.",
			printMaxSVIDTTL(sc.CATTL), printMinCATTL(sc.SVIDTTL),
		)
		msgSVIDTTLTooLargeAndCATTLTooSmall := fmt.Sprintf(
			"The default_svid_ttl is too high and the ca_ttl is too low. "+
				"SVIDs with shorter lifetimes may be issued. "+
				"Please set the default_svid_ttl to %v or less, and the ca_ttl to %v or more, "+
				"to guarantee the full default_svid_ttl lifetime when CA rotations are scheduled.",
			printDuration(ca.MaxSVIDTTL()), printMinCATTL(ca.MaxSVIDTTL()),
		)
		msgSVIDTTLTooLarge := fmt.Sprintf(
			"The default_svid_ttl is too high. "+
				"SVIDs with shorter lifetimes may be issued. "+
				"Please set the default_svid_ttl to %v or less "+
				"to guarantee the full default_svid_ttl lifetime when CA rotations are scheduled.",
			printMaxSVIDTTL(sc.CATTL),
		)

		switch {
		case sc.SVIDTTL < ca.MaxSVIDTTL():
			// The SVID TTL is smaller than our cap, but the CA TTL
			// is not large enough to accommodate it
			sc.Log.Warn(msgCATTLTooSmall)
		case sc.CATTL < ca.MinCATTLForSVIDTTL(ca.MaxSVIDTTL()):
			// The SVID TTL is larger than our cap, it needs to be
			// decreased no matter what. Additionally, the CA TTL is
			// too small to accommodate the maximum SVID TTL.
			sc.Log.Warn(msgSVIDTTLTooLargeAndCATTLTooSmall)
		default:
			// The SVID TTL is larger than our cap and needs to be
			// decreased.
			sc.Log.Warn(msgSVIDTTLTooLarge)
		}
	}

	if c.Server.CAKeyType != "" {
		keyType, err := keyTypeFromString(c.Server.CAKeyType)
		if err != nil {
			return nil, fmt.Errorf("error parsing ca_key_type: %w", err)
		}
		sc.CAKeyType = keyType
		sc.JWTKeyType = keyType
	} else {
		sc.CAKeyType = keymanager.ECP256
		sc.JWTKeyType = keymanager.ECP256
	}

	if c.Server.JWTKeyType != "" {
		sc.JWTKeyType, err = keyTypeFromString(c.Server.JWTKeyType)
		if err != nil {
			return nil, fmt.Errorf("error parsing jwt_key_type: %w", err)
		}
	}

	sc.JWTIssuer = c.Server.JWTIssuer

	if subject := c.Server.CASubject; subject != nil {
		sc.CASubject = pkix.Name{
			Organization: subject.Organization,
			Country:      subject.Country,
			CommonName:   subject.CommonName,
		}
		if isPKIXNameEmpty(sc.CASubject) {
			sc.Log.Warn("ca_subject configurable is set but empty; the default will be used")
		}
	}
	// RFC3280(4.1.2.4) requires the issuer DN be set.
	if isPKIXNameEmpty(sc.CASubject) {
		sc.CASubject = defaultCASubject
	}

	sc.PluginConfigs = *c.Plugins
	sc.Telemetry = c.Telemetry
	sc.HealthChecks = c.HealthChecks

	if !allowUnknownConfig {
		if err := checkForUnknownConfig(c, sc.Log); err != nil {
			return nil, err
		}
	}

	if c.Server.Experimental.CacheReloadInterval != "" {
		interval, err := time.ParseDuration(c.Server.Experimental.CacheReloadInterval)
		if err != nil {
			return nil, fmt.Errorf("could not parse cache reload interval: %w", err)
		}
		sc.CacheReloadInterval = interval
	}

	sc.AuthOpaPolicyEngineConfig = c.Server.Experimental.AuthOpaPolicyEngine

	return sc, nil
}

func parseBundleEndpointProfile(config federatesWithConfig) (trustDomainConfig *bundleClient.TrustDomainConfig, err error) {
	// First check the number of bundle endpoint profiles in the config
	objectList, ok := config.BundleEndpointProfile.(*ast.ObjectList)
	if !ok {
		return nil, errors.New("malformed configuration")
	}
	if len(objectList.Items) != 1 {
		return nil, errors.New("exactly one bundle endpoint profile is expected")
	}

	// Parse the configuration
	var data bytes.Buffer
	if err := printer.DefaultConfig.Fprint(&data, config.BundleEndpointProfile); err != nil {
		return nil, err
	}
	configString := data.String()
	profileConfig := new(bundleEndpointProfileConfig)
	if err := hcl.Decode(profileConfig, configString); err != nil {
		return nil, fmt.Errorf("failed to decode configuration: %w", err)
	}

	var endpointProfile bundleClient.EndpointProfileInfo
	switch {
	case profileConfig.HTTPSWeb != nil:
		endpointProfile = bundleClient.HTTPSWebProfile{}
	case profileConfig.HTTPSSPIFFE != nil:
		spiffeID, err := spiffeid.FromString(profileConfig.HTTPSSPIFFE.EndpointSPIFFEID)
		if err != nil {
			return nil, fmt.Errorf("could not get endpoint SPIFFE ID: %w", err)
		}
		endpointProfile = bundleClient.HTTPSSPIFFEProfile{EndpointSPIFFEID: spiffeID}
	default:
		return nil, errors.New(`no bundle endpoint profile defined; current supported profiles are "https_spiffe" and 'https_web"`)
	}

	return &bundleClient.TrustDomainConfig{
		EndpointURL:     config.BundleEndpointURL,
		EndpointProfile: endpointProfile,
	}, nil
}

func parseDeprecatedBundleEndpoint(config *deprecatedFederatesWithBundleEndpointConfig) (trustDomainConfig *bundleClient.TrustDomainConfig, err error) {
	port := defaultBundleEndpointPort
	if config.Port != 0 {
		port = config.Port
	}
	if config.UseWebPKI && config.SpiffeID != "" {
		return nil, errors.New("usage of `bundle_endpoint.spiffe_id` is not allowed when authenticating with Web PKI")
	}

	var endpointProfile bundleClient.EndpointProfileInfo
	if config.UseWebPKI {
		endpointProfile = bundleClient.HTTPSWebProfile{}
	} else {
		var spiffeID spiffeid.ID
		if config.SpiffeID != "" {
			spiffeID, err = spiffeid.FromString(config.SpiffeID)
			if err != nil {
				return nil, fmt.Errorf("could not parse endpoint SPIFFE ID %q: %w", config.SpiffeID, err)
			}
		}

		endpointProfile = bundleClient.HTTPSSPIFFEProfile{EndpointSPIFFEID: spiffeID}
	}

	return &bundleClient.TrustDomainConfig{
		DeprecatedConfig: true,
		EndpointURL:      fmt.Sprintf("https://%s:%d", config.Address, port),
		EndpointProfile:  endpointProfile,
	}, nil
}

func validateConfig(c *Config) error {
	if c.Server == nil {
		return errors.New("server section must be configured")
	}

	if c.Server.BindAddress == "" || c.Server.BindPort == 0 {
		return errors.New("bind_address and bind_port must be configured")
	}

	if c.Server.SocketPath != "" && c.Server.DeprecatedRegistrationUDSPath != "" {
		return errors.New("socket_path and the deprecated registration_uds_path are mutually exclusive")
	}

	if c.Server.TrustDomain == "" {
		return errors.New("trust_domain must be configured")
	}

	if c.Server.DataDir == "" {
		return errors.New("data_dir must be configured")
	}

	if c.Plugins == nil {
		return errors.New("plugins section must be configured")
	}

	if c.Server.Federation != nil {
		if c.Server.Federation.BundleEndpoint != nil &&
			c.Server.Federation.BundleEndpoint.ACME != nil {
			acme := c.Server.Federation.BundleEndpoint.ACME

			if acme.DomainName == "" {
				return errors.New("federation.bundle_endpoint.acme.domain_name must be configured")
			}

			if acme.Email == "" {
				return errors.New("federation.bundle_endpoint.acme.email must be configured")
			}
		}

		for td, tdConfig := range c.Server.Federation.FederatesWith {
			switch {
			case tdConfig.DeprecatedBundleEndpoint != nil:
				if tdConfig.DeprecatedBundleEndpoint.Address == "" {
					return fmt.Errorf("federation.federates_with[\"%s\"].bundle_endpoint.address must be configured", td)
				}
			case tdConfig.BundleEndpointURL == "":
				return fmt.Errorf("federation.federates_with[\"%s\"].bundle_endpoint_url must be configured", td)
			case !strings.HasPrefix(strings.ToLower(tdConfig.BundleEndpointURL), "https://"):
				return fmt.Errorf("federation.federates_with[\"%s\"].bundle_endpoint_url must use the HTTPS protocol; URL found: %q", td, tdConfig.BundleEndpointURL)
			}
		}
	}

	return nil
}

func checkForUnknownConfig(c *Config, l logrus.FieldLogger) (err error) {
	detectedUnknown := func(section string, keys []string) {
		l.WithFields(logrus.Fields{
			"section": section,
			"keys":    strings.Join(keys, ","),
		}).Error("Unknown configuration detected")
		err = errors.New("unknown configuration detected")
	}

	if len(c.UnusedKeys) != 0 {
		detectedUnknown("top-level", c.UnusedKeys)
	}

	if c.Server != nil {
		if len(c.Server.UnusedKeys) != 0 {
			detectedUnknown("server", c.Server.UnusedKeys)
		}

		if cs := c.Server.CASubject; cs != nil && len(cs.UnusedKeys) != 0 {
			detectedUnknown("ca_subject", cs.UnusedKeys)
		}

		if rl := c.Server.RateLimit; len(rl.UnusedKeys) != 0 {
			detectedUnknown("ratelimit", rl.UnusedKeys)
		}

		// TODO: Re-enable unused key detection for experimental config. See
		// https://github.com/spiffe/spire/issues/1101 for more information
		//
		// if len(c.Server.Experimental.UnusedKeys) != 0 {
		//	detectedUnknown("experimental", c.Server.Experimental.UnusedKeys)
		// }

		if c.Server.Federation != nil {
			// TODO: Re-enable unused key detection for federation config. See
			// https://github.com/spiffe/spire/issues/1101 for more information
			//
			// if len(c.Server.Federation.UnusedKeys) != 0 {
			//	detectedUnknown("federation", c.Server.Federation.UnusedKeys)
			// }

			if c.Server.Federation.BundleEndpoint != nil {
				if len(c.Server.Federation.BundleEndpoint.UnusedKeys) != 0 {
					detectedUnknown("bundle endpoint", c.Server.Federation.BundleEndpoint.UnusedKeys)
				}

				if bea := c.Server.Federation.BundleEndpoint.ACME; bea != nil && len(bea.UnusedKeys) != 0 {
					detectedUnknown("bundle endpoint ACME", bea.UnusedKeys)
				}
			}

			// TODO: Re-enable unused key detection for bundle endpoint profile config. See
			// https://github.com/spiffe/spire/issues/1101 for more information
			//
			// for k, v := range c.Server.Federation.FederatesWith {
			//	if len(v.UnusedKeys) != 0 {
			//		detectedUnknown(fmt.Sprintf("federates_with %q", k), v.UnusedKeys)
			//	}
			// }
		}
	}

	// TODO: Re-enable unused key detection for telemetry. See
	// https://github.com/spiffe/spire/issues/1101 for more information
	//
	// if len(c.Telemetry.UnusedKeys) != 0 {
	//	detectedUnknown("telemetry", c.Telemetry.UnusedKeys)
	// }

	if p := c.Telemetry.Prometheus; p != nil && len(p.UnusedKeys) != 0 {
		detectedUnknown("Prometheus", p.UnusedKeys)
	}

	for _, v := range c.Telemetry.DogStatsd {
		if len(v.UnusedKeys) != 0 {
			detectedUnknown("DogStatsd", v.UnusedKeys)
		}
	}

	for _, v := range c.Telemetry.Statsd {
		if len(v.UnusedKeys) != 0 {
			detectedUnknown("Statsd", v.UnusedKeys)
		}
	}

	for _, v := range c.Telemetry.M3 {
		if len(v.UnusedKeys) != 0 {
			detectedUnknown("M3", v.UnusedKeys)
		}
	}

	if p := c.Telemetry.InMem; p != nil && len(p.UnusedKeys) != 0 {
		detectedUnknown("InMem", p.UnusedKeys)
	}

	if len(c.HealthChecks.UnusedKeys) != 0 {
		detectedUnknown("health check", c.HealthChecks.UnusedKeys)
	}

	return err
}

func defaultConfig() *Config {
	return &Config{
		Server: &serverConfig{
			BindAddress:    "0.0.0.0",
			BindPort:       8081,
			CATTL:          ca.DefaultCATTL.String(),
			LogLevel:       defaultLogLevel,
			LogFormat:      log.DefaultFormat,
			DefaultSVIDTTL: ca.DefaultX509SVIDTTL.String(),
			Experimental:   experimentalConfig{},
		},
	}
}

func keyTypeFromString(s string) (keymanager.KeyType, error) {
	switch strings.ToLower(s) {
	case "rsa-2048":
		return keymanager.RSA2048, nil
	case "rsa-4096":
		return keymanager.RSA4096, nil
	case "ec-p256":
		return keymanager.ECP256, nil
	case "ec-p384":
		return keymanager.ECP384, nil
	default:
		return keymanager.KeyTypeUnset, fmt.Errorf("key type %q is unknown; must be one of [rsa-2048, rsa-4096, ec-p256, ec-p384]", s)
	}
}

// hasCompatibleTTLs checks if we can guarantee the configured SVID TTL given the
// configurd CA TTL. If we detect that a new SVIDs TTL may be cut short due to
// a scheduled CA rotation, this function will return false.
func hasCompatibleTTLs(caTTL, svidTTL time.Duration) bool {
	return ca.MaxSVIDTTLForCATTL(caTTL) >= svidTTL
}

// printMaxSVIDTTL calculates the display string for a sufficiently short SVID TTL
func printMaxSVIDTTL(caTTL time.Duration) string {
	return printDuration(ca.MaxSVIDTTLForCATTL(caTTL))
}

// printMinCATTL calculates the display string for a sufficiently large CA TTL
func printMinCATTL(svidTTL time.Duration) string {
	return printDuration(ca.MinCATTLForSVIDTTL(svidTTL))
}

func printDuration(d time.Duration) string {
	s := d.Truncate(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

func isPKIXNameEmpty(name pkix.Name) bool {
	// pkix.Name contains slices which make it directly incomparable. We could
	// do a field by field check since it is unlikely that pkix.Name will grow,
	// but reflect.DeepEqual is more convenient and safe for this particular
	// use.
	return reflect.DeepEqual(name, pkix.Name{})
}
