/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package sdk narrows the UpCloud Go SDK down to the calls this provider makes, so that the rest of
// the code depends on an interface that can be faked in tests.
package sdk

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/client"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/service"
)

// UpCloudAPI is the subset of the UpCloud API used by this provider.
type UpCloudAPI interface {
	service.Cloud
	service.Server
	service.ServerGroup
	service.Storage

	// GetServersWithFilters is not part of service.Server but is the only way to list servers by
	// label, which is how Karpenter-managed servers are discovered.
	GetServersWithFilters(ctx context.Context, r *request.GetServersWithFiltersRequest) (*upcloud.Servers, error)
}

// *service.Service implements UpCloudAPI.
var _ UpCloudAPI = (*service.Service)(nil)

const defaultClientTimeout = 30 * time.Second

// ClientConfig holds the credentials and tuning knobs for the UpCloud API client. Exactly one of
// Token or Username/Password must be set; UpCloud API tokens are preferred because they can be
// scoped down to the permissions this controller actually needs.
type ClientConfig struct {
	Token    string
	Username string
	Password string
	Timeout  time.Duration
	BaseURL  string
}

// ClientConfigFromEnv reads the standard UpCloud environment variables. These are the same names
// the UpCloud CLI and Terraform provider use, so a Secret written for one works for all of them.
func ClientConfigFromEnv() (ClientConfig, error) {
	config := ClientConfig{
		Token:    os.Getenv(client.EnvToken),
		Username: os.Getenv(client.EnvUsername),
		Password: os.Getenv(client.EnvPassword),
	}
	if config.Token != "" && (config.Username != "" || config.Password != "") {
		return ClientConfig{}, fmt.Errorf("only one authentication method may be provided, set either %s or %s and %s",
			client.EnvToken, client.EnvUsername, client.EnvPassword)
	}
	if config.Token == "" && (config.Username == "" || config.Password == "") {
		return ClientConfig{}, fmt.Errorf("no credentials found, set %s or both %s and %s",
			client.EnvToken, client.EnvUsername, client.EnvPassword)
	}
	if raw, ok := os.LookupEnv("UPCLOUD_CLIENT_TIMEOUT"); ok {
		timeout, err := time.ParseDuration(raw)
		if err != nil {
			return ClientConfig{}, fmt.Errorf("parsing UPCLOUD_CLIENT_TIMEOUT %q, %w", raw, err)
		}
		config.Timeout = timeout
	}
	return config, nil
}

// NewClient builds an UpCloud API service from the supplied configuration.
func NewClient(config ClientConfig, userAgent string) (UpCloudAPI, error) {
	timeout := defaultClientTimeout
	if config.Timeout > 0 {
		timeout = config.Timeout
	}
	opts := []client.ConfigFn{
		client.WithHTTPClient(&http.Client{Timeout: timeout}),
		client.WithTimeout(timeout),
	}
	switch {
	case config.Token != "":
		opts = append(opts, client.WithBearerAuth(config.Token))
	case config.Username != "" && config.Password != "":
		opts = append(opts, client.WithBasicAuth(config.Username, config.Password))
	default:
		return nil, errors.New("no UpCloud credentials configured")
	}
	if config.BaseURL != "" {
		opts = append(opts, client.WithBaseURL(config.BaseURL))
	}

	c := client.New("", "", opts...)
	c.UserAgent = userAgent
	return service.New(c), nil
}

// IsNotFound reports whether err is UpCloud's way of saying the resource is gone. UpCloud returns
// several distinct error codes for this depending on the resource, and a bare 404 for some
// endpoints, so all of them are folded together here.
func IsNotFound(err error) bool {
	problem := &upcloud.Problem{}
	if !errors.As(err, &problem) {
		return false
	}
	if problem.Status == http.StatusNotFound {
		return true
	}
	switch problem.ErrorCode() {
	case upcloud.ErrCodeServerNotFound, upcloud.ErrCodeResourceNotFound,
		upcloud.ErrCodeNotFound, upcloud.ErrCodeStorageNotFound:
		return true
	}
	return false
}

// IsInsufficientCapacity reports whether err means UpCloud could not satisfy the request right now
// but might later. These are the errors that should take an offering out of rotation rather than
// fail the NodeClaim outright.
func IsInsufficientCapacity(err error) bool {
	problem := &upcloud.Problem{}
	if !errors.As(err, &problem) {
		return false
	}
	switch problem.ErrorCode() {
	case upcloud.ErrCodeServerResourcesUnavailable,
		upcloud.ErrCodeStorageResourcesUnavailable,
		upcloud.ErrCodeIpAddressResourcesUnavailable,
		upcloud.ErrCodeServerCreatingLimitReached,
		upcloud.ErrCodeServerCoresLimitReached,
		upcloud.ErrCodeServerMemoryLimitReached,
		upcloud.ErrCodeMaxiOpsStorageLimitReached,
		upcloud.ErrCodeStrictAntiAffinityNotMet,
		upcloud.ErrCodeZoneHostForbidden:
		return true
	}
	return false
}

// IsRetryable reports whether err is a transient transport or server-side failure that is worth
// retrying against the same offering.
func IsRetryable(err error) bool {
	problem := &upcloud.Problem{}
	if !errors.As(err, &problem) {
		// Transport-level failures surface as plain errors; the controller-runtime requeue path
		// retries those anyway, so only classify what we can positively identify.
		return false
	}
	switch problem.Status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}
