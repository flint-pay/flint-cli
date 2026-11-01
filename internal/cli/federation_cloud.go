package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/endpointcreds"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

func gcpEnvironment() bool {
	return os.Getenv("K_SERVICE") != "" || os.Getenv("FUNCTION_TARGET") != "" || os.Getenv("GCE_METADATA_HOST") != ""
}

func azureEnvironment() bool {
	return os.Getenv("IDENTITY_ENDPOINT") != "" && os.Getenv("IDENTITY_HEADER") != ""
}

func awsEnvironment() bool {
	return strings.HasPrefix(os.Getenv("AWS_EXECUTION_ENV"), "AWS_Lambda_") || strings.HasPrefix(os.Getenv("AWS_EXECUTION_ENV"), "AWS_ECS_") || os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != "" || os.Getenv("ECS_CONTAINER_METADATA_URI") != "" || os.Getenv("ECS_CONTAINER_METADATA_URI_V4") != ""
}

// Do not let the SDK's inbound federation provider read a foreign audience file,
// including web_identity_token_file in a shared profile. Other default providers
// (environment, shared profiles, SSO, ECS and EC2 roles) remain SDK-managed.
type refusedAWSIdentityFile struct{}

func (refusedAWSIdentityFile) GetIdentityToken() ([]byte, error) {
	return nil, errors.New("inbound web identity token files cannot supply Flint outbound identity")
}

func (c federationConfig) cloudSubjectToken(ctx context.Context) (string, *CLIError) {
	unavailable := func(message string) (string, *CLIError) {
		return "", c.failure("FEDERATION_SOURCE_UNAVAILABLE", subjectClaims{}, message)
	}
	failed := func(message string) (string, *CLIError) {
		return "", c.failure("OIDC_TOKEN_REQUEST_FAILED", subjectClaims{}, message)
	}
	// Bound the entire SDK credential discovery and identity request, including
	// retries. Use a separate copy so API exchange retains its existing timeout.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client := *federationHTTPClient
	client.Timeout = 5 * time.Second
	endpoint, header, value := "", "", ""
	query := url.Values{}
	switch c.Source {
	case "gcp":
		endpoint = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity"
		header, value = "Metadata-Flavor", "Google"
		query.Set("audience", c.Audience)
		query.Set("format", "full")
	case "azure":
		if strings.TrimSpace(os.Getenv("FLINT_OIDC_AUDIENCE")) == "" {
			return unavailable("Set FLINT_OIDC_AUDIENCE to your Entra app ID URI for Azure managed identity tokens.")
		}
		endpoint = os.Getenv("IDENTITY_ENDPOINT")
		if (endpoint == "") != (os.Getenv("IDENTITY_HEADER") == "") {
			return unavailable("Azure requires IDENTITY_ENDPOINT and IDENTITY_HEADER together. Enable the App Service or Container Apps managed identity, or unset both to use VM IMDS.")
		}
		if endpoint != "" {
			header, value = "X-IDENTITY-HEADER", os.Getenv("IDENTITY_HEADER")
			query.Set("api-version", "2019-08-01")
		} else {
			endpoint = "http://169.254.169.254/metadata/identity/oauth2/token"
			header, value = "Metadata", "true"
			query.Set("api-version", "2018-02-01")
		}
		query.Set("resource", c.Audience)
		if id := strings.TrimSpace(os.Getenv("FLINT_AZURE_CLIENT_ID")); id != "" {
			query.Set("client_id", id)
		}
	case "aws":
		cfg, err := awsconfig.LoadDefaultConfig(ctx,
			awsconfig.WithHTTPClient(&client), awsconfig.WithRetryMaxAttempts(1),
			// Unlike IMDS and service clients, endpointcreds does not inherit
			// cfg.HTTPClient in this SDK version.
			awsconfig.WithEndpointCredentialOptions(func(o *endpointcreds.Options) {
				o.HTTPClient = &client
			}),
			awsconfig.WithWebIdentityRoleCredentialOptions(func(o *stscreds.WebIdentityRoleOptions) {
				o.TokenRetriever = refusedAWSIdentityFile{}
			}),
		)
		if err != nil || cfg.Region == "" {
			return unavailable("AWS configuration is unavailable. Set AWS_REGION or configure a regional profile and a workload role for STS GetWebIdentityToken.")
		}
		out, err := sts.NewFromConfig(cfg).GetWebIdentityToken(ctx, &sts.GetWebIdentityTokenInput{
			Audience: []string{c.Audience}, SigningAlgorithm: aws.String("ES384"),
		})
		if err != nil || out == nil || aws.ToString(out.WebIdentityToken) == "" {
			return failed("AWS STS GetWebIdentityToken failed. Check the regional endpoint, workload credentials and sts:GetWebIdentityToken permission. Inbound AWS_WEB_IDENTITY_TOKEN_FILE and profile web identity token files are not used.")
		}
		return aws.ToString(out.WebIdentityToken), nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
		return unavailable("The cloud identity endpoint is invalid. Check the platform-provided identity endpoint configuration.")
	}
	q := u.Query()
	for key, values := range query {
		q[key] = values
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return unavailable("The cloud identity endpoint is invalid. Check the platform-provided identity endpoint configuration.")
	}
	req.Header.Set(header, value)
	resp, err := client.Do(req)
	if err != nil {
		return failed("The cloud identity request failed. Check metadata access and managed identity configuration; redirects are refused.")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 || resp.StatusCode != http.StatusOK {
		return failed("The cloud identity endpoint did not return a token. Check metadata access, managed identity permissions and FLINT_OIDC_AUDIENCE.")
	}
	token := strings.TrimSpace(string(raw))
	if c.Source == "azure" {
		var body struct {
			AccessToken string `json:"access_token"`
		}
		if json.Unmarshal(raw, &body) != nil {
			return failed("Azure returned an invalid identity response. Check managed identity configuration and FLINT_OIDC_AUDIENCE.")
		}
		token = body.AccessToken
	}
	if token == "" {
		return failed("The cloud identity endpoint returned an empty token. Check managed identity configuration and FLINT_OIDC_AUDIENCE.")
	}
	return token, nil
}
