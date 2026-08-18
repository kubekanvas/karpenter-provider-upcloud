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

// Package fake provides an in-memory UpCloud API for tests.
package fake

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"

	sdk "github.com/kubekanvas/karpenter-provider-upcloud/pkg/upcloud"
)

// UpCloudAPI is a fake implementation of sdk.UpCloudAPI.
//
// It embeds the interface rather than implementing all of it: every method the provider does not
// call is nil, so calling one panics loudly instead of silently returning a zero value and letting
// a test pass for the wrong reason.
type UpCloudAPI struct {
	sdk.UpCloudAPI

	mu sync.Mutex

	// Responses. Zero values are usable: an empty Servers list, no plans, and so on.
	Plans          []upcloud.Plan
	Prices         upcloud.PricesByZone
	Zones          []upcloud.Zone
	Storages       map[string]upcloud.StorageDetails
	ServerGroups   map[string]upcloud.ServerGroup
	Servers        map[string]*upcloud.ServerDetails
	CreatedServer  *upcloud.ServerDetails
	ListedServers  []upcloud.Server
	NextServerUUID string

	// Errors, injected per call. An *upcloud.Problem here exercises the sdk error classifiers.
	GetPlansErr       error
	GetPricesErr      error
	GetZonesErr       error
	GetStorageErr     error
	GetServerGroupErr error
	CreateServerErr   error
	GetServerErr      error
	ListServersErr    error
	StopServerErr     error
	DeleteServerErr   error
	ModifyServerErr   error

	// Recorded calls, for asserting what the provider actually did.
	CreateServerCalls []request.CreateServerRequest
	StopServerCalls   []request.StopServerRequest
	DeleteCalls       []request.DeleteServerAndStoragesRequest
	ModifyCalls       []request.ModifyServerRequest
	ListFilterCalls   [][]request.QueryFilter
}

// NewUpCloudAPI returns a fake with initialized maps.
func NewUpCloudAPI() *UpCloudAPI {
	return &UpCloudAPI{
		Storages:     map[string]upcloud.StorageDetails{},
		ServerGroups: map[string]upcloud.ServerGroup{},
		Servers:      map[string]*upcloud.ServerDetails{},
		Prices:       upcloud.PricesByZone{},
	}
}

// Problem builds an UpCloud API error with the given status and error code, so tests can drive the
// not-found / insufficient-capacity / retryable classifiers in pkg/upcloud.
func Problem(status int, errorCode string) *upcloud.Problem {
	return &upcloud.Problem{Status: status, Type: errorCode, Title: errorCode}
}

// NotFound is the error UpCloud returns for a server that no longer exists.
func NotFound() *upcloud.Problem {
	return Problem(http.StatusNotFound, upcloud.ErrCodeServerNotFound)
}

func (f *UpCloudAPI) GetPlans(_ context.Context) (*upcloud.Plans, error) {
	if f.GetPlansErr != nil {
		return nil, f.GetPlansErr
	}
	return &upcloud.Plans{Plans: f.Plans}, nil
}

func (f *UpCloudAPI) GetPricesByZone(_ context.Context) (*upcloud.PricesByZone, error) {
	if f.GetPricesErr != nil {
		return nil, f.GetPricesErr
	}
	prices := f.Prices
	return &prices, nil
}

func (f *UpCloudAPI) GetZones(_ context.Context) (*upcloud.Zones, error) {
	if f.GetZonesErr != nil {
		return nil, f.GetZonesErr
	}
	return &upcloud.Zones{Zones: f.Zones}, nil
}

func (f *UpCloudAPI) GetStorageDetails(_ context.Context, r *request.GetStorageDetailsRequest) (*upcloud.StorageDetails, error) {
	if f.GetStorageErr != nil {
		return nil, f.GetStorageErr
	}
	s, ok := f.Storages[r.UUID]
	if !ok {
		return nil, Problem(http.StatusNotFound, upcloud.ErrCodeStorageNotFound)
	}
	return &s, nil
}

func (f *UpCloudAPI) GetServerGroup(_ context.Context, r *request.GetServerGroupRequest) (*upcloud.ServerGroup, error) {
	if f.GetServerGroupErr != nil {
		return nil, f.GetServerGroupErr
	}
	g, ok := f.ServerGroups[r.UUID]
	if !ok {
		return nil, Problem(http.StatusNotFound, upcloud.ErrCodeResourceNotFound)
	}
	return &g, nil
}

func (f *UpCloudAPI) CreateServer(_ context.Context, r *request.CreateServerRequest) (*upcloud.ServerDetails, error) {
	f.mu.Lock()
	f.CreateServerCalls = append(f.CreateServerCalls, *r)
	f.mu.Unlock()

	if f.CreateServerErr != nil {
		return nil, f.CreateServerErr
	}
	if f.CreatedServer != nil {
		return f.CreatedServer, nil
	}

	uuid := f.NextServerUUID
	if uuid == "" {
		uuid = fmt.Sprintf("fake-%d", len(f.CreateServerCalls))
	}
	details := &upcloud.ServerDetails{
		Server: upcloud.Server{
			UUID:     uuid,
			Hostname: r.Hostname,
			Title:    r.Title,
			Zone:     r.Zone,
			Plan:     r.Plan,
			State:    upcloud.ServerStateStarted,
		},
	}
	if r.Labels != nil {
		details.Labels = *r.Labels
	}
	f.mu.Lock()
	f.Servers[uuid] = details
	f.mu.Unlock()
	return details, nil
}

func (f *UpCloudAPI) GetServerDetails(_ context.Context, r *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error) {
	if f.GetServerErr != nil {
		return nil, f.GetServerErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.Servers[r.UUID]
	if !ok {
		return nil, NotFound()
	}
	return s, nil
}

func (f *UpCloudAPI) GetServersWithFilters(_ context.Context, r *request.GetServersWithFiltersRequest) (*upcloud.Servers, error) {
	f.mu.Lock()
	f.ListFilterCalls = append(f.ListFilterCalls, r.Filters)
	f.mu.Unlock()

	if f.ListServersErr != nil {
		return nil, f.ListServersErr
	}
	if f.ListedServers != nil {
		return &upcloud.Servers{Servers: f.ListedServers}, nil
	}
	servers := make([]upcloud.Server, 0, len(f.Servers))
	for _, s := range f.Servers {
		servers = append(servers, s.Server)
	}
	return &upcloud.Servers{Servers: servers}, nil
}

func (f *UpCloudAPI) StopServer(_ context.Context, r *request.StopServerRequest) (*upcloud.ServerDetails, error) {
	f.mu.Lock()
	f.StopServerCalls = append(f.StopServerCalls, *r)
	f.mu.Unlock()

	if f.StopServerErr != nil {
		return nil, f.StopServerErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.Servers[r.UUID]
	if !ok {
		return nil, NotFound()
	}
	// UpCloud moves a stopping server through "maintenance" before it reaches "stopped".
	s.State = upcloud.ServerStateMaintenance
	return s, nil
}

func (f *UpCloudAPI) DeleteServerAndStorages(_ context.Context, r *request.DeleteServerAndStoragesRequest) error {
	f.mu.Lock()
	f.DeleteCalls = append(f.DeleteCalls, *r)
	f.mu.Unlock()

	if f.DeleteServerErr != nil {
		return f.DeleteServerErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Servers[r.UUID]; !ok {
		return NotFound()
	}
	delete(f.Servers, r.UUID)
	return nil
}

func (f *UpCloudAPI) ModifyServer(_ context.Context, r *request.ModifyServerRequest) (*upcloud.ServerDetails, error) {
	f.mu.Lock()
	f.ModifyCalls = append(f.ModifyCalls, *r)
	f.mu.Unlock()

	if f.ModifyServerErr != nil {
		return nil, f.ModifyServerErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.Servers[r.UUID]
	if !ok {
		return nil, NotFound()
	}
	if r.Labels != nil {
		s.Labels = *r.Labels
	}
	return s, nil
}

// SetServerState forces a server into a given lifecycle state, for driving the Delete state machine.
func (f *UpCloudAPI) SetServerState(uuid, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.Servers[uuid]; ok {
		s.State = state
	}
}

// AddServer registers an existing server with the given labels.
func (f *UpCloudAPI) AddServer(uuid, plan, zone, state string, labels map[string]string) {
	ls := make(upcloud.LabelSlice, 0, len(labels))
	for k, v := range labels {
		ls = append(ls, upcloud.Label{Key: k, Value: v})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Servers[uuid] = &upcloud.ServerDetails{
		Server: upcloud.Server{UUID: uuid, Plan: plan, Zone: zone, State: state, Hostname: uuid},
		Labels: ls,
	}
}
