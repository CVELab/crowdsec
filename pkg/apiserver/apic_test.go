package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/go-openapi/strfmt"
	"github.com/jarcoal/httpmock"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/tomb.v2"

	"github.com/crowdsecurity/go-cs-lib/cstest"
	"github.com/crowdsecurity/go-cs-lib/ptr"
	"github.com/crowdsecurity/go-cs-lib/version"

	"github.com/crowdsecurity/crowdsec/pkg/apiclient"
	"github.com/crowdsecurity/crowdsec/pkg/csconfig"
	"github.com/crowdsecurity/crowdsec/pkg/database"
	"github.com/crowdsecurity/crowdsec/pkg/database/ent/decision"
	"github.com/crowdsecurity/crowdsec/pkg/database/ent/machine"
	"github.com/crowdsecurity/crowdsec/pkg/models"
	"github.com/crowdsecurity/crowdsec/pkg/modelscapi"
	"github.com/crowdsecurity/crowdsec/pkg/types"
)

func getDBClient(t *testing.T, ctx context.Context) *database.Client {
	t.Helper()

	dbPath, err := os.CreateTemp("", "*sqlite")
	require.NoError(t, err)
	dbClient, err := database.NewClient(ctx, &csconfig.DatabaseCfg{
		Type:   "sqlite",
		DbName: "crowdsec",
		DbPath: dbPath.Name(),
	})
	require.NoError(t, err)

	return dbClient
}

func getAPIC(t *testing.T, ctx context.Context) *apic {
	t.Helper()
	dbClient := getDBClient(t, ctx)

	return &apic{
		AlertsAddChan: make(chan []*models.Alert),
		// DecisionDeleteChan: make(chan []*models.Decision),
		dbClient:    dbClient,
		mu:          sync.Mutex{},
		startup:     true,
		pullTomb:    tomb.Tomb{},
		pushTomb:    tomb.Tomb{},
		metricsTomb: tomb.Tomb{},
		consoleConfig: &csconfig.ConsoleConfig{
			ShareManualDecisions:  ptr.Of(false),
			ShareTaintedScenarios: ptr.Of(false),
			ShareCustomScenarios:  ptr.Of(false),
			ShareContext:          ptr.Of(false),
		},
		isPulling:      make(chan bool, 1),
		shareSignals:   true,
		pullBlocklists: true,
		pullCommunity:  true,
	}
}

func absDiff(a int, b int) int {
	c := a - b
	if c < 0 {
		return -1 * c
	}

	return c
}

func assertTotalDecisionCount(t *testing.T, ctx context.Context, dbClient *database.Client, count int) {
	d := dbClient.Ent.Decision.Query().AllX(ctx)
	assert.Len(t, d, count)
}

func assertTotalValidDecisionCount(t *testing.T, dbClient *database.Client, count int) {
	ctx := t.Context()
	d := dbClient.Ent.Decision.Query().Where(
		decision.UntilGT(time.Now()),
	).AllX(ctx)
	assert.Len(t, d, count)
}

func jsonMarshalX(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}

	return data
}

func assertTotalAlertCount(t *testing.T, dbClient *database.Client, count int) {
	ctx := t.Context()
	d := dbClient.Ent.Alert.Query().AllX(ctx)
	assert.Len(t, d, count)
}

func TestAPICCAPIPullIsOld(t *testing.T) {
	ctx := t.Context()
	api := getAPIC(t, ctx)

	isOld, err := api.CAPIPullIsOld(ctx)
	require.NoError(t, err)
	assert.True(t, isOld)

	decision := api.dbClient.Ent.Decision.Create().
		SetUntil(time.Now().Add(time.Hour)).
		SetScenario("crowdsec/test").
		SetType("IP").
		SetScope("Country").
		SetValue("Blah").
		SetOrigin(types.CAPIOrigin).
		SaveX(ctx)

	api.dbClient.Ent.Alert.Create().
		SetCreatedAt(time.Now()).
		SetScenario("crowdsec/test").
		AddDecisions(
			decision,
		).
		SaveX(ctx)

	isOld, err = api.CAPIPullIsOld(ctx)
	require.NoError(t, err)

	assert.False(t, isOld)
}

func TestAPICFetchScenariosListFromDB(t *testing.T) {
	ctx := t.Context()

	tests := []struct {
		name                    string
		machineIDsWithScenarios map[string]string
		expectedScenarios       []string
	}{
		{
			name: "Simple one machine with two scenarios",
			machineIDsWithScenarios: map[string]string{
				"a": "crowdsecurity/http-bf,crowdsecurity/ssh-bf",
			},
			expectedScenarios: []string{"crowdsecurity/ssh-bf", "crowdsecurity/http-bf"},
		},
		{
			name: "Multi machine with custom+hub scenarios",
			machineIDsWithScenarios: map[string]string{
				"a": "crowdsecurity/http-bf,crowdsecurity/ssh-bf,my_scenario",
				"b": "crowdsecurity/http-bf,crowdsecurity/ssh-bf,foo_scenario",
			},
			expectedScenarios: []string{"crowdsecurity/ssh-bf", "crowdsecurity/http-bf", "my_scenario", "foo_scenario"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api := getAPIC(t, ctx)
			for machineID, scenarios := range tc.machineIDsWithScenarios {
				api.dbClient.Ent.Machine.Create().
					SetMachineId(machineID).
					SetPassword(testPassword.String()).
					SetIpAddress("1.2.3.4").
					SetScenarios(scenarios).
					ExecX(ctx)
			}

			scenarios, err := api.FetchScenariosListFromDB(ctx)
			require.NoError(t, err)

			for machineID := range tc.machineIDsWithScenarios {
				api.dbClient.Ent.Machine.Delete().Where(machine.MachineIdEQ(machineID)).ExecX(ctx)
			}

			assert.ElementsMatch(t, tc.expectedScenarios, scenarios)
		})
	}
}

func TestNewAPIC(t *testing.T) {
	ctx := t.Context()

	var testConfig *csconfig.OnlineApiClientCfg

	setConfig := func() {
		testConfig = &csconfig.OnlineApiClientCfg{
			Credentials: &csconfig.ApiCredentialsCfg{
				URL:      "http://foobar/",
				Login:    "foo",
				Password: "bar",
			},
			Sharing: ptr.Of(true),
			PullConfig: csconfig.CapiPullConfig{
				Community:  ptr.Of(true),
				Blocklists: ptr.Of(true),
			},
		}
	}

	type args struct {
		dbClient      *database.Client
		consoleConfig *csconfig.ConsoleConfig
	}

	tests := []struct {
		name        string
		args        args
		expectedErr string
		action      func()
	}{
		{
			name:   "simple",
			action: func() {},
			args: args{
				dbClient:      getDBClient(t, ctx),
				consoleConfig: LoadTestConfig(t).API.Server.ConsoleConfig,
			},
		},
		{
			name:   "error in parsing URL",
			action: func() { testConfig.Credentials.URL = "foobar http://" },
			args: args{
				dbClient:      getDBClient(t, ctx),
				consoleConfig: LoadTestConfig(t).API.Server.ConsoleConfig,
			},
			expectedErr: "first path segment in URL cannot contain colon",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setConfig()
			httpmock.Activate()

			defer httpmock.DeactivateAndReset()
			httpmock.RegisterResponder("POST", "http://foobar/v3/watchers/login", httpmock.NewBytesResponder(
				200, jsonMarshalX(
					models.WatcherAuthResponse{
						Code:   200,
						Expire: "2023-01-12T22:51:43Z",
						Token:  "MyToken",
					},
				),
			))
			tc.action()
			_, err := NewAPIC(ctx, testConfig, tc.args.dbClient, tc.args.consoleConfig, nil)
			cstest.RequireErrorContains(t, err, tc.expectedErr)
		})
	}
}

func TestAPICGetMetrics(t *testing.T) {
	ctx := t.Context()

	cleanUp := func(api *apic) {
		api.dbClient.Ent.Bouncer.Delete().ExecX(ctx)
		api.dbClient.Ent.Machine.Delete().ExecX(ctx)
	}
	tests := []struct {
		name           string
		machineIDs     []string
		bouncers       []string
		expectedMetric *models.Metrics
	}{
		{
			name:       "no bouncers nor machines should still have bouncers/machines keys in output",
			machineIDs: []string{},
			bouncers:   []string{},
			expectedMetric: &models.Metrics{
				ApilVersion: ptr.Of(version.String()),
				Bouncers:    []*models.MetricsBouncerInfo{},
				Machines:    []*models.MetricsAgentInfo{},
			},
		},
		{
			name:       "simple",
			machineIDs: []string{"a", "b", "c"},
			bouncers:   []string{"1", "2", "3"},
			expectedMetric: &models.Metrics{
				ApilVersion: ptr.Of(version.String()),
				Bouncers: []*models.MetricsBouncerInfo{
					{
						CustomName: "1",
						LastPull:   time.Time{}.Format(time.RFC3339),
					}, {
						CustomName: "2",
						LastPull:   time.Time{}.Format(time.RFC3339),
					}, {
						CustomName: "3",
						LastPull:   time.Time{}.Format(time.RFC3339),
					},
				},
				Machines: []*models.MetricsAgentInfo{
					{
						Name:       "a",
						LastPush:   time.Time{}.Format(time.RFC3339),
						LastUpdate: time.Time{}.Format(time.RFC3339),
					},
					{
						Name:       "b",
						LastPush:   time.Time{}.Format(time.RFC3339),
						LastUpdate: time.Time{}.Format(time.RFC3339),
					},
					{
						Name:       "c",
						LastPush:   time.Time{}.Format(time.RFC3339),
						LastUpdate: time.Time{}.Format(time.RFC3339),
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			apiClient := getAPIC(t, ctx)
			cleanUp(apiClient)

			for i, machineID := range tc.machineIDs {
				apiClient.dbClient.Ent.Machine.Create().
					SetMachineId(machineID).
					SetPassword(testPassword.String()).
					SetIpAddress(fmt.Sprintf("1.2.3.%d", i)).
					SetScenarios("crowdsecurity/test").
					SetLastPush(time.Time{}).
					SetUpdatedAt(time.Time{}).
					ExecX(ctx)
			}

			for i, bouncerName := range tc.bouncers {
				apiClient.dbClient.Ent.Bouncer.Create().
					SetIPAddress(fmt.Sprintf("1.2.3.%d", i)).
					SetName(bouncerName).
					SetAPIKey("foobar").
					SetRevoked(false).
					SetLastPull(time.Time{}).
					ExecX(ctx)
			}

			foundMetrics, err := apiClient.GetMetrics(ctx)
			require.NoError(t, err)

			assert.Equal(t, tc.expectedMetric.Bouncers, foundMetrics.Bouncers)
			assert.Equal(t, tc.expectedMetric.Machines, foundMetrics.Machines)
		})
	}
}

func TestCreateAlertsForDecision(t *testing.T) {
	httpBfDecisionList := &models.Decision{
		Origin:   ptr.Of(types.ListOrigin),
		Scenario: ptr.Of("crowdsecurity/http-bf"),
	}

	sshBfDecisionList := &models.Decision{
		Origin:   ptr.Of(types.ListOrigin),
		Scenario: ptr.Of("crowdsecurity/ssh-bf"),
	}

	httpBfDecisionCommunity := &models.Decision{
		Origin:   ptr.Of(types.CAPIOrigin),
		Scenario: ptr.Of("crowdsecurity/http-bf"),
	}

	sshBfDecisionCommunity := &models.Decision{
		Origin:   ptr.Of(types.CAPIOrigin),
		Scenario: ptr.Of("crowdsecurity/ssh-bf"),
	}

	type args struct {
		decisions []*models.Decision
	}

	tests := []struct {
		name string
		args args
		want []*models.Alert
	}{
		{
			name: "2 decisions CAPI List Decisions should create 2 alerts",
			args: args{
				decisions: []*models.Decision{
					httpBfDecisionList,
					sshBfDecisionList,
				},
			},
			want: []*models.Alert{
				createAlertForDecision(httpBfDecisionList),
				createAlertForDecision(sshBfDecisionList),
			},
		},
		{
			name: "2 decisions CAPI List same scenario decisions should create 1 alert",
			args: args{
				decisions: []*models.Decision{
					httpBfDecisionList,
					httpBfDecisionList,
				},
			},
			want: []*models.Alert{
				createAlertForDecision(httpBfDecisionList),
			},
		},
		{
			name: "5 decisions from community list should create 1 alert",
			args: args{
				decisions: []*models.Decision{
					httpBfDecisionCommunity,
					httpBfDecisionCommunity,
					sshBfDecisionCommunity,
					sshBfDecisionCommunity,
					sshBfDecisionCommunity,
				},
			},
			want: []*models.Alert{
				createAlertForDecision(sshBfDecisionCommunity),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := createAlertsForDecisions(tc.args.decisions); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("createAlertsForDecisions() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFillAlertsWithDecisions(t *testing.T) {
	httpBfDecisionCommunity := &models.Decision{
		Origin:   ptr.Of(types.CAPIOrigin),
		Scenario: ptr.Of("crowdsecurity/http-bf"),
		Scope:    ptr.Of("ip"),
	}

	sshBfDecisionCommunity := &models.Decision{
		Origin:   ptr.Of(types.CAPIOrigin),
		Scenario: ptr.Of("crowdsecurity/ssh-bf"),
		Scope:    ptr.Of("ip"),
	}

	httpBfDecisionList := &models.Decision{
		Origin:   ptr.Of(types.ListOrigin),
		Scenario: ptr.Of("crowdsecurity/http-bf"),
		Scope:    ptr.Of("ip"),
	}

	sshBfDecisionList := &models.Decision{
		Origin:   ptr.Of(types.ListOrigin),
		Scenario: ptr.Of("crowdsecurity/ssh-bf"),
		Scope:    ptr.Of("ip"),
	}

	type args struct {
		alerts    []*models.Alert
		decisions []*models.Decision
	}

	tests := []struct {
		name string
		args args
		want []*models.Alert
	}{
		{
			name: "1 CAPI alert should pair up with n CAPI decisions",
			args: args{
				alerts:    []*models.Alert{createAlertForDecision(httpBfDecisionCommunity)},
				decisions: []*models.Decision{httpBfDecisionCommunity, sshBfDecisionCommunity, sshBfDecisionCommunity, httpBfDecisionCommunity},
			},
			want: []*models.Alert{
				func() *models.Alert {
					a := createAlertForDecision(httpBfDecisionCommunity)
					a.Decisions = []*models.Decision{httpBfDecisionCommunity, sshBfDecisionCommunity, sshBfDecisionCommunity, httpBfDecisionCommunity}
					return a
				}(),
			},
		},
		{
			name: "List alert should pair up only with decisions having same scenario",
			args: args{
				alerts:    []*models.Alert{createAlertForDecision(httpBfDecisionList), createAlertForDecision(sshBfDecisionList)},
				decisions: []*models.Decision{httpBfDecisionList, httpBfDecisionList, sshBfDecisionList, sshBfDecisionList},
			},
			want: []*models.Alert{
				func() *models.Alert {
					a := createAlertForDecision(httpBfDecisionList)
					a.Decisions = []*models.Decision{httpBfDecisionList, httpBfDecisionList}
					return a
				}(),
				func() *models.Alert {
					a := createAlertForDecision(sshBfDecisionList)
					a.Decisions = []*models.Decision{sshBfDecisionList, sshBfDecisionList}
					return a
				}(),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			addCounters, _ := makeAddAndDeleteCounters()
			if got := fillAlertsWithDecisions(tc.args.alerts, tc.args.decisions, addCounters); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("fillAlertsWithDecisions() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAPICWhitelists(t *testing.T) {
	ctx := t.Context()
	api := getAPIC(t, ctx)
	// one whitelist on IP, one on CIDR
	api.whitelists = &csconfig.CapiWhitelist{}
	api.whitelists.Ips = append(api.whitelists.Ips, netip.MustParseAddr("9.2.3.4"), netip.MustParseAddr("7.2.3.4"))

	tnet, err := netip.ParsePrefix("13.2.3.0/24")
	require.NoError(t, err)

	api.whitelists.Cidrs = append(api.whitelists.Cidrs, tnet)

	tnet, err = netip.ParsePrefix("11.2.3.0/24")
	require.NoError(t, err)

	api.whitelists.Cidrs = append(api.whitelists.Cidrs, tnet)

	api.dbClient.Ent.Decision.Create().
		SetOrigin(types.CAPIOrigin).
		SetType("ban").
		SetValue("9.9.9.9").
		SetScope("Ip").
		SetScenario("crowdsecurity/ssh-bf").
		SetUntil(time.Now().Add(time.Hour)).
		ExecX(ctx)
	assertTotalDecisionCount(t, ctx, api.dbClient, 1)
	assertTotalValidDecisionCount(t, api.dbClient, 1)
	httpmock.Activate()

	defer httpmock.DeactivateAndReset()
	httpmock.RegisterResponder("GET", "http://api.crowdsec.net/api/decisions/stream", httpmock.NewBytesResponder(
		200, jsonMarshalX(
			modelscapi.GetDecisionsStreamResponse{
				Deleted: modelscapi.GetDecisionsStreamResponseDeleted{
					&modelscapi.GetDecisionsStreamResponseDeletedItem{
						Decisions: []string{
							"9.9.9.9", // This is already present in DB
							"9.1.9.9", // This is not present in DB
						},
						Scope: ptr.Of("Ip"),
					}, // This is already present in DB
				},
				New: modelscapi.GetDecisionsStreamResponseNew{
					&modelscapi.GetDecisionsStreamResponseNewItem{
						Scenario: ptr.Of("crowdsecurity/test1"),
						Scope:    ptr.Of("Ip"),
						Decisions: []*modelscapi.GetDecisionsStreamResponseNewItemDecisionsItems0{
							{
								Value:    ptr.Of("13.2.3.4"), // wl by cidr
								Duration: ptr.Of("24h"),
							},
						},
					},

					&modelscapi.GetDecisionsStreamResponseNewItem{
						Scenario: ptr.Of("crowdsecurity/test1"),
						Scope:    ptr.Of("Ip"),
						Decisions: []*modelscapi.GetDecisionsStreamResponseNewItemDecisionsItems0{
							{
								Value:    ptr.Of("2.2.3.4"),
								Duration: ptr.Of("24h"),
							},
						},
					},
					&modelscapi.GetDecisionsStreamResponseNewItem{
						Scenario: ptr.Of("crowdsecurity/test2"),
						Scope:    ptr.Of("Ip"),
						Decisions: []*modelscapi.GetDecisionsStreamResponseNewItemDecisionsItems0{
							{
								Value:    ptr.Of("13.2.3.5"), // wl by cidr
								Duration: ptr.Of("24h"),
							},
						},
					}, // These two are from community list.
					&modelscapi.GetDecisionsStreamResponseNewItem{
						Scenario: ptr.Of("crowdsecurity/test1"),
						Scope:    ptr.Of("Ip"),
						Decisions: []*modelscapi.GetDecisionsStreamResponseNewItemDecisionsItems0{
							{
								Value:    ptr.Of("6.2.3.4"),
								Duration: ptr.Of("24h"),
							},
						},
					},
					&modelscapi.GetDecisionsStreamResponseNewItem{
						Scenario: ptr.Of("crowdsecurity/test1"),
						Scope:    ptr.Of("Ip"),
						Decisions: []*modelscapi.GetDecisionsStreamResponseNewItemDecisionsItems0{
							{
								Value:    ptr.Of("9.2.3.4"), // wl by ip
								Duration: ptr.Of("24h"),
							},
						},
					},
					&modelscapi.GetDecisionsStreamResponseNewItem{
						Scenario: ptr.Of("crowdsecurity/test1"),
						Scope:    ptr.Of("Ip"),
						Decisions: []*modelscapi.GetDecisionsStreamResponseNewItemDecisionsItems0{
							{
								Value:    ptr.Of("10.2.3.4"), // wl by allowlist that we pull at the same time
								Duration: ptr.Of("24h"),
							},
						},
					},
				},
				Links: &modelscapi.GetDecisionsStreamResponseLinks{
					Blocklists: []*modelscapi.BlocklistLink{
						{
							URL:         ptr.Of("http://api.crowdsec.net/blocklist1"),
							Name:        ptr.Of("blocklist1"),
							Scope:       ptr.Of("Ip"),
							Remediation: ptr.Of("ban"),
							Duration:    ptr.Of("24h"),
						},
						{
							URL:         ptr.Of("http://api.crowdsec.net/blocklist2"),
							Name:        ptr.Of("blocklist2"),
							Scope:       ptr.Of("Ip"),
							Remediation: ptr.Of("ban"),
							Duration:    ptr.Of("24h"),
						},
					},
					Allowlists: []*modelscapi.AllowlistLink{
						{
							URL:         ptr.Of("http://api.crowdsec.net/allowlist1"),
							Name:        ptr.Of("allowlist1"),
							ID:          ptr.Of("1"),
							Description: ptr.Of("test"),
							CreatedAt:   ptr.Of(strfmt.DateTime(time.Now())),
						},
					},
				},
			},
		),
	))

	httpmock.RegisterResponder("GET", "http://api.crowdsec.net/blocklist1", httpmock.NewStringResponder(
		200, "1.2.3.6",
	))

	httpmock.RegisterResponder("GET", "http://api.crowdsec.net/blocklist2", httpmock.NewStringResponder(
		200, "1.2.3.7",
	))

	httpmock.RegisterResponder("GET", "http://api.crowdsec.net/allowlist1", httpmock.NewStringResponder(
		200, `{"value":"10.2.3.4"}`,
	))

	url, err := url.ParseRequestURI("http://api.crowdsec.net/")
	require.NoError(t, err)

	apic, err := apiclient.NewDefaultClient(
		url,
		"/api",
		"",
		nil,
	)
	require.NoError(t, err)

	api.apiClient = apic
	err = api.PullTop(ctx, false)
	require.NoError(t, err)

	allowlists, err := api.dbClient.ListAllowLists(ctx, true)
	require.NoError(t, err)

	require.Len(t, allowlists, 1)
	require.Equal(t, "allowlist1", allowlists[0].Name)
	require.Equal(t, "test", allowlists[0].Description)
	require.True(t, allowlists[0].FromConsole)

	assertTotalDecisionCount(t, ctx, api.dbClient, 5) // 2 from FIRE + 2 from bl + 1 existing
	assertTotalValidDecisionCount(t, api.dbClient, 4)
	assertTotalAlertCount(t, api.dbClient, 3) // 2 for list sub , 1 for community list.
	alerts := api.dbClient.Ent.Alert.Query().AllX(ctx)
	validDecisions := api.dbClient.Ent.Decision.Query().Where(
		decision.UntilGT(time.Now())).
		AllX(ctx)

	decisionScenarioFreq := make(map[string]int)
	decisionIP := make(map[string]int)

	alertScenario := make(map[string]int)

	for _, alert := range alerts {
		alertScenario[alert.SourceScope]++
	}

	assert.Len(t, alertScenario, 3)
	assert.Equal(t, 1, alertScenario[types.CommunityBlocklistPullSourceScope])
	assert.Equal(t, 1, alertScenario["lists:blocklist1"])
	assert.Equal(t, 1, alertScenario["lists:blocklist2"])

	for _, decisions := range validDecisions {
		decisionScenarioFreq[decisions.Scenario]++
		decisionIP[decisions.Value]++
	}

	assert.Equal(t, 1, decisionIP["2.2.3.4"], 1)
	assert.Equal(t, 1, decisionIP["6.2.3.4"], 1)

	if _, ok := decisionIP["13.2.3.4"]; ok {
		t.Errorf("13.2.3.4 is whitelisted")
	}

	if _, ok := decisionIP["13.2.3.5"]; ok {
		t.Errorf("13.2.3.5 is whitelisted")
	}

	if _, ok := decisionIP["9.2.3.4"]; ok {
		t.Errorf("9.2.3.4 is whitelisted")
	}

	if _, ok := decisionIP["10.2.3.4"]; ok {
		t.Errorf("10.2.3.4 is whitelisted")
	}

	assert.Equal(t, 1, decisionScenarioFreq["blocklist1"], 1)
	assert.Equal(t, 1, decisionScenarioFreq["blocklist2"], 1)
	assert.Equal(t, 2, decisionScenarioFreq["crowdsecurity/test1"], 2)
}

func TestAPICPullTop(t *testing.T) {
	ctx := t.Context()
	api := getAPIC(t, ctx)
	api.dbClient.Ent.Decision.Create().
		SetOrigin(types.CAPIOrigin).
		SetType("ban").
		SetValue("9.9.9.9").
		SetScope("Ip").
		SetScenario("crowdsecurity/ssh-bf").
		SetUntil(time.Now().Add(time.Hour)).
		ExecX(ctx)
	assertTotalDecisionCount(t, ctx, api.dbClient, 1)
	assertTotalValidDecisionCount(t, api.dbClient, 1)
	httpmock.Activate()

	defer httpmock.DeactivateAndReset()
	httpmock.RegisterResponder("GET", "http://api.crowdsec.net/api/decisions/stream", httpmock.NewBytesResponder(
		200, jsonMarshalX(
			modelscapi.GetDecisionsStreamResponse{
				Deleted: modelscapi.GetDecisionsStreamResponseDeleted{
					&modelscapi.GetDecisionsStreamResponseDeletedItem{
						Decisions: []string{
							"9.9.9.9", // This is already present in DB
							"9.1.9.9", // This is not present in DB
						},
						Scope: ptr.Of("Ip"),
					}, // This is already present in DB
				},
				New: modelscapi.GetDecisionsStreamResponseNew{
					&modelscapi.GetDecisionsStreamResponseNewItem{
						Scenario: ptr.Of("crowdsecurity/test1"),
						Scope:    ptr.Of("Ip"),
						Decisions: []*modelscapi.GetDecisionsStreamResponseNewItemDecisionsItems0{
							{
								Value:    ptr.Of("1.2.3.4"),
								Duration: ptr.Of("24h"),
							},
						},
					},
					&modelscapi.GetDecisionsStreamResponseNewItem{
						Scenario: ptr.Of("crowdsecurity/test2"),
						Scope:    ptr.Of("Ip"),
						Decisions: []*modelscapi.GetDecisionsStreamResponseNewItemDecisionsItems0{
							{
								Value:    ptr.Of("1.2.3.5"),
								Duration: ptr.Of("24h"),
							},
						},
					}, // These two are from community list.
				},
				Links: &modelscapi.GetDecisionsStreamResponseLinks{
					Blocklists: []*modelscapi.BlocklistLink{
						{
							URL:         ptr.Of("http://api.crowdsec.net/blocklist1"),
							Name:        ptr.Of("blocklist1"),
							Scope:       ptr.Of("Ip"),
							Remediation: ptr.Of("ban"),
							Duration:    ptr.Of("24h"),
						},
						{
							URL:         ptr.Of("http://api.crowdsec.net/blocklist2"),
							Name:        ptr.Of("blocklist2"),
							Scope:       ptr.Of("Ip"),
							Remediation: ptr.Of("ban"),
							Duration:    ptr.Of("24h"),
						},
					},
				},
			},
		),
	))

	httpmock.RegisterResponder("GET", "http://api.crowdsec.net/blocklist1", httpmock.NewStringResponder(
		200, "1.2.3.6",
	))

	httpmock.RegisterResponder("GET", "http://api.crowdsec.net/blocklist2", httpmock.NewStringResponder(
		200, "1.2.3.7",
	))

	url, err := url.ParseRequestURI("http://api.crowdsec.net/")
	require.NoError(t, err)

	apic, err := apiclient.NewDefaultClient(
		url,
		"/api",
		"",
		nil,
	)
	require.NoError(t, err)

	api.apiClient = apic
	err = api.PullTop(ctx, false)
	require.NoError(t, err)

	assertTotalDecisionCount(t, ctx, api.dbClient, 5)
	assertTotalValidDecisionCount(t, api.dbClient, 4)
	assertTotalAlertCount(t, api.dbClient, 3) // 2 for list sub , 1 for community list.
	alerts := api.dbClient.Ent.Alert.Query().AllX(ctx)
	validDecisions := api.dbClient.Ent.Decision.Query().Where(
		decision.UntilGT(time.Now())).
		AllX(ctx)

	decisionScenarioFreq := make(map[string]int)
	alertScenario := make(map[string]int)

	for _, alert := range alerts {
		alertScenario[alert.SourceScope]++
	}

	assert.Len(t, alertScenario, 3)
	assert.Equal(t, 1, alertScenario[types.CommunityBlocklistPullSourceScope])
	assert.Equal(t, 1, alertScenario["lists:blocklist1"])
	assert.Equal(t, 1, alertScenario["lists:blocklist2"])

	for _, decisions := range validDecisions {
		decisionScenarioFreq[decisions.Scenario]++
	}

	assert.Equal(t, 1, decisionScenarioFreq["blocklist1"], 1)
	assert.Equal(t, 1, decisionScenarioFreq["blocklist2"], 1)
	assert.Equal(t, 1, decisionScenarioFreq["crowdsecurity/test1"], 1)
	assert.Equal(t, 1, decisionScenarioFreq["crowdsecurity/test2"], 1)
}

func TestAPICPullTopBLCacheFirstCall(t *testing.T) {
	ctx := t.Context()
	// no decision in db, no last modified parameter.
	api := getAPIC(t, ctx)

	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	httpmock.RegisterResponder("GET", "http://api.crowdsec.net/api/decisions/stream", httpmock.NewBytesResponder(
		200, jsonMarshalX(
			modelscapi.GetDecisionsStreamResponse{
				New: modelscapi.GetDecisionsStreamResponseNew{
					&modelscapi.GetDecisionsStreamResponseNewItem{
						Scenario: ptr.Of("crowdsecurity/test1"),
						Scope:    ptr.Of("Ip"),
						Decisions: []*modelscapi.GetDecisionsStreamResponseNewItemDecisionsItems0{
							{
								Value:    ptr.Of("1.2.3.4"),
								Duration: ptr.Of("24h"),
							},
						},
					},
				},
				Links: &modelscapi.GetDecisionsStreamResponseLinks{
					Blocklists: []*modelscapi.BlocklistLink{
						{
							URL:         ptr.Of("http://api.crowdsec.net/blocklist1"),
							Name:        ptr.Of("blocklist1"),
							Scope:       ptr.Of("Ip"),
							Remediation: ptr.Of("ban"),
							Duration:    ptr.Of("24h"),
						},
					},
				},
			},
		),
	))

	httpmock.RegisterResponder("GET", "http://api.crowdsec.net/blocklist1", func(req *http.Request) (*http.Response, error) {
		assert.Empty(t, req.Header.Get("If-Modified-Since"))
		return httpmock.NewStringResponse(200, "1.2.3.4"), nil
	})

	url, err := url.ParseRequestURI("http://api.crowdsec.net/")
	require.NoError(t, err)

	apic, err := apiclient.NewDefaultClient(
		url,
		"/api",
		"",
		nil,
	)
	require.NoError(t, err)

	api.apiClient = apic
	err = api.PullTop(ctx, false)
	require.NoError(t, err)

	blocklistConfigItemName := "blocklist:blocklist1:last_pull"
	lastPullTimestamp, err := api.dbClient.GetConfigItem(ctx, blocklistConfigItemName)
	require.NoError(t, err)
	assert.NotEmpty(t, lastPullTimestamp)

	// new call should return 304 and should not change lastPullTimestamp
	httpmock.RegisterResponder("GET", "http://api.crowdsec.net/blocklist1", func(req *http.Request) (*http.Response, error) {
		assert.NotEmpty(t, req.Header.Get("If-Modified-Since"))
		return httpmock.NewStringResponse(304, ""), nil
	})

	err = api.PullTop(ctx, false)
	require.NoError(t, err)
	secondLastPullTimestamp, err := api.dbClient.GetConfigItem(ctx, blocklistConfigItemName)
	require.NoError(t, err)
	assert.Equal(t, lastPullTimestamp, secondLastPullTimestamp)
}

func TestAPICPullTopBLCacheForceCall(t *testing.T) {
	ctx := t.Context()
	api := getAPIC(t, ctx)

	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	// create a decision about to expire. It should force fetch
	alertInstance := api.dbClient.Ent.Alert.
		Create().
		SetScenario("update list").
		SetSourceScope("list:blocklist1").
		SetSourceValue("list:blocklist1").
		SaveX(ctx)

	api.dbClient.Ent.Decision.Create().
		SetOrigin(types.ListOrigin).
		SetType("ban").
		SetValue("9.9.9.9").
		SetScope("Ip").
		SetScenario("blocklist1").
		SetUntil(time.Now().Add(time.Hour)).
		SetOwnerID(alertInstance.ID).
		ExecX(ctx)

	httpmock.RegisterResponder("GET", "http://api.crowdsec.net/api/decisions/stream", httpmock.NewBytesResponder(
		200, jsonMarshalX(
			modelscapi.GetDecisionsStreamResponse{
				New: modelscapi.GetDecisionsStreamResponseNew{
					&modelscapi.GetDecisionsStreamResponseNewItem{
						Scenario: ptr.Of("crowdsecurity/test1"),
						Scope:    ptr.Of("Ip"),
						Decisions: []*modelscapi.GetDecisionsStreamResponseNewItemDecisionsItems0{
							{
								Value:    ptr.Of("1.2.3.4"),
								Duration: ptr.Of("24h"),
							},
						},
					},
				},
				Links: &modelscapi.GetDecisionsStreamResponseLinks{
					Blocklists: []*modelscapi.BlocklistLink{
						{
							URL:         ptr.Of("http://api.crowdsec.net/blocklist1"),
							Name:        ptr.Of("blocklist1"),
							Scope:       ptr.Of("Ip"),
							Remediation: ptr.Of("ban"),
							Duration:    ptr.Of("24h"),
						},
					},
				},
			},
		),
	))

	httpmock.RegisterResponder("GET", "http://api.crowdsec.net/blocklist1", func(req *http.Request) (*http.Response, error) {
		assert.Empty(t, req.Header.Get("If-Modified-Since"))
		return httpmock.NewStringResponse(304, ""), nil
	})

	url, err := url.ParseRequestURI("http://api.crowdsec.net/")
	require.NoError(t, err)

	apic, err := apiclient.NewDefaultClient(
		url,
		"/api",
		"",
		nil,
	)
	require.NoError(t, err)

	api.apiClient = apic
	err = api.PullTop(ctx, false)
	require.NoError(t, err)
}

func TestAPICPullBlocklistCall(t *testing.T) {
	ctx := t.Context()
	api := getAPIC(t, ctx)

	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	httpmock.RegisterResponder("GET", "http://api.crowdsec.net/blocklist1", func(req *http.Request) (*http.Response, error) {
		assert.Empty(t, req.Header.Get("If-Modified-Since"))
		return httpmock.NewStringResponse(200, "1.2.3.4"), nil
	})

	url, err := url.ParseRequestURI("http://api.crowdsec.net/")
	require.NoError(t, err)

	apic, err := apiclient.NewDefaultClient(
		url,
		"/api",
		"",
		nil,
	)
	require.NoError(t, err)

	api.apiClient = apic
	err = api.PullBlocklist(ctx, &modelscapi.BlocklistLink{
		URL:         ptr.Of("http://api.crowdsec.net/blocklist1"),
		Name:        ptr.Of("blocklist1"),
		Scope:       ptr.Of("Ip"),
		Remediation: ptr.Of("ban"),
		Duration:    ptr.Of("24h"),
	}, true)
	require.NoError(t, err)
}

func TestAPICPush(t *testing.T) {
	ctx := t.Context()
	tests := []struct {
		name          string
		alerts        []*models.Alert
		expectedCalls int
	}{
		{
			name: "simple single alert",
			alerts: []*models.Alert{
				{
					Scenario:        ptr.Of("crowdsec/test"),
					ScenarioHash:    ptr.Of("certified"),
					ScenarioVersion: ptr.Of("v1.0"),
					Simulated:       ptr.Of(false),
					Source:          &models.Source{},
				},
			},
			expectedCalls: 1,
		},
		{
			name: "simulated alert is not pushed",
			alerts: []*models.Alert{
				{
					Scenario:        ptr.Of("crowdsec/test"),
					ScenarioHash:    ptr.Of("certified"),
					ScenarioVersion: ptr.Of("v1.0"),
					Simulated:       ptr.Of(true),
					Source:          &models.Source{},
				},
			},
			expectedCalls: 0,
		},
		{
			name:          "1 request per 50 alerts",
			expectedCalls: 2,
			alerts: func() []*models.Alert {
				alerts := make([]*models.Alert, 100)
				for i := range 100 {
					alerts[i] = &models.Alert{
						Scenario:        ptr.Of("crowdsec/test"),
						ScenarioHash:    ptr.Of("certified"),
						ScenarioVersion: ptr.Of("v1.0"),
						Simulated:       ptr.Of(false),
						Source:          &models.Source{},
					}
				}

				return alerts
			}(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api := getAPIC(t, ctx)
			api.pushInterval = time.Millisecond
			api.pushIntervalFirst = time.Millisecond
			url, err := url.ParseRequestURI("http://api.crowdsec.net/")
			require.NoError(t, err)

			httpmock.Activate()
			defer httpmock.DeactivateAndReset()

			apic, err := apiclient.NewDefaultClient(
				url,
				"/api",
				"",
				nil,
			)
			require.NoError(t, err)

			api.apiClient = apic

			httpmock.RegisterResponder("POST", "http://api.crowdsec.net/api/signals", httpmock.NewBytesResponder(200, []byte{}))

			// capture the alerts to avoid datarace
			alerts := tc.alerts
			go func() {
				api.AlertsAddChan <- alerts

				time.Sleep(time.Second)
				api.Shutdown()
			}()

			err = api.Push(ctx)
			require.NoError(t, err)
			assert.Equal(t, tc.expectedCalls, httpmock.GetTotalCallCount())
		})
	}
}

func TestAPICPull(t *testing.T) {
	ctx := t.Context()
	api := getAPIC(t, ctx)
	tests := []struct {
		name                  string
		setUp                 func()
		expectedDecisionCount int
		logContains           string
	}{
		{
			name:        "test pull if no scenarios are present",
			setUp:       func() {},
			logContains: "scenario list is empty, will not pull yet",
		},
		{
			name: "test pull",
			setUp: func() {
				api.dbClient.Ent.Machine.Create().
					SetMachineId("1.2.3.4").
					SetPassword(testPassword.String()).
					SetIpAddress("1.2.3.4").
					SetScenarios("crowdsecurity/ssh-bf").
					ExecX(ctx)
			},
			expectedDecisionCount: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api = getAPIC(t, ctx)
			api.pullInterval = time.Millisecond
			api.pullIntervalFirst = time.Millisecond
			url, err := url.ParseRequestURI("http://api.crowdsec.net/")
			require.NoError(t, err)
			httpmock.Activate()

			defer httpmock.DeactivateAndReset()

			apic, err := apiclient.NewDefaultClient(
				url,
				"/api",
				"",
				nil,
			)
			require.NoError(t, err)

			api.apiClient = apic

			httpmock.RegisterNoResponder(httpmock.NewBytesResponder(200, jsonMarshalX(
				modelscapi.GetDecisionsStreamResponse{
					New: modelscapi.GetDecisionsStreamResponseNew{
						&modelscapi.GetDecisionsStreamResponseNewItem{
							Scenario: ptr.Of("crowdsecurity/ssh-bf"),
							Scope:    ptr.Of("Ip"),
							Decisions: []*modelscapi.GetDecisionsStreamResponseNewItemDecisionsItems0{
								{
									Value:    ptr.Of("1.2.3.5"),
									Duration: ptr.Of("24h"),
								},
							},
						},
					},
				},
			)))
			tc.setUp()

			var buf bytes.Buffer

			go func() {
				logrus.SetOutput(&buf)

				if err := api.Pull(ctx); err != nil {
					panic(err)
				}
			}()

			// Slightly long because the CI runner for windows are slow, and this can lead to random failure
			time.Sleep(time.Millisecond * 500)
			logrus.SetOutput(os.Stderr)
			assert.Contains(t, buf.String(), tc.logContains)
			assertTotalDecisionCount(t, ctx, api.dbClient, tc.expectedDecisionCount)
		})
	}
}

func TestShouldShareAlert(t *testing.T) {
	tests := []struct {
		name          string
		consoleConfig *csconfig.ConsoleConfig
		shareSignals  bool
		alert         *models.Alert
		expectedRet   bool
		expectedTrust string
	}{
		{
			name: "custom alert should be shared if config enables it",
			consoleConfig: &csconfig.ConsoleConfig{
				ShareCustomScenarios: ptr.Of(true),
			},
			alert:         &models.Alert{Simulated: ptr.Of(false)},
			shareSignals:  true,
			expectedRet:   true,
			expectedTrust: "custom",
		},
		{
			name: "custom alert should not be shared if config disables it",
			consoleConfig: &csconfig.ConsoleConfig{
				ShareCustomScenarios: ptr.Of(false),
			},
			alert:         &models.Alert{Simulated: ptr.Of(false)},
			shareSignals:  true,
			expectedRet:   false,
			expectedTrust: "custom",
		},
		{
			name: "manual alert should be shared if config enables it",
			consoleConfig: &csconfig.ConsoleConfig{
				ShareManualDecisions: ptr.Of(true),
			},
			shareSignals: true,
			alert: &models.Alert{
				Simulated: ptr.Of(false),
				Decisions: []*models.Decision{{Origin: ptr.Of(types.CscliOrigin)}},
			},
			expectedRet:   true,
			expectedTrust: "manual",
		},
		{
			name: "manual alert should not be shared if config disables it",
			consoleConfig: &csconfig.ConsoleConfig{
				ShareManualDecisions: ptr.Of(false),
			},
			shareSignals: true,
			alert: &models.Alert{
				Simulated: ptr.Of(false),
				Decisions: []*models.Decision{{Origin: ptr.Of(types.CscliOrigin)}},
			},
			expectedRet:   false,
			expectedTrust: "manual",
		},
		{
			name: "manual alert should be shared if config enables it",
			consoleConfig: &csconfig.ConsoleConfig{
				ShareTaintedScenarios: ptr.Of(true),
			},
			shareSignals: true,
			alert: &models.Alert{
				Simulated:    ptr.Of(false),
				ScenarioHash: ptr.Of("whateverHash"),
			},
			expectedRet:   true,
			expectedTrust: "tainted",
		},
		{
			name: "manual alert should not be shared if config disables it",
			consoleConfig: &csconfig.ConsoleConfig{
				ShareTaintedScenarios: ptr.Of(false),
			},
			shareSignals: true,
			alert: &models.Alert{
				Simulated:    ptr.Of(false),
				ScenarioHash: ptr.Of("whateverHash"),
			},
			expectedRet:   false,
			expectedTrust: "tainted",
		},
		{
			name: "manual alert should not be shared if global sharing is disabled",
			consoleConfig: &csconfig.ConsoleConfig{
				ShareManualDecisions: ptr.Of(true),
			},
			shareSignals: false,
			alert: &models.Alert{
				Simulated:    ptr.Of(false),
				ScenarioHash: ptr.Of("whateverHash"),
			},
			expectedRet:   false,
			expectedTrust: "manual",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ret := shouldShareAlert(tc.alert, tc.consoleConfig, tc.shareSignals)
			assert.Equal(t, tc.expectedRet, ret)
		})
	}
}
