package apiserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/go-openapi/strfmt"
	log "github.com/sirupsen/logrus"
	"gopkg.in/tomb.v2"

	"github.com/crowdsecurity/go-cs-lib/ptr"
	"github.com/crowdsecurity/go-cs-lib/trace"

	"github.com/crowdsecurity/crowdsec/pkg/apiclient"
	"github.com/crowdsecurity/crowdsec/pkg/csconfig"
	"github.com/crowdsecurity/crowdsec/pkg/database"
	"github.com/crowdsecurity/crowdsec/pkg/database/ent"
	"github.com/crowdsecurity/crowdsec/pkg/database/ent/alert"
	"github.com/crowdsecurity/crowdsec/pkg/database/ent/decision"
	"github.com/crowdsecurity/crowdsec/pkg/models"
	"github.com/crowdsecurity/crowdsec/pkg/modelscapi"
	"github.com/crowdsecurity/crowdsec/pkg/types"
)

const (
	// delta values must be smaller than the interval
	pullIntervalDefault       = time.Hour * 2
	pullIntervalDelta         = time.Minute * 5
	pushIntervalDefault       = time.Second * 10
	pushIntervalDelta         = time.Second * 7
	metricsIntervalDefault    = time.Minute * 30
	metricsIntervalDelta      = time.Minute * 15
	usageMetricsInterval      = time.Minute * 30
	usageMetricsIntervalDelta = time.Minute * 15
)

type apic struct {
	// when changing the intervals in tests, always set *First too
	// or they can be negative
	pullInterval              time.Duration
	pullIntervalFirst         time.Duration
	pushInterval              time.Duration
	pushIntervalFirst         time.Duration
	metricsInterval           time.Duration
	metricsIntervalFirst      time.Duration
	usageMetricsInterval      time.Duration
	usageMetricsIntervalFirst time.Duration
	dbClient                  *database.Client
	apiClient                 *apiclient.ApiClient
	AlertsAddChan             chan []*models.Alert

	mu            sync.Mutex
	pushTomb      tomb.Tomb
	pullTomb      tomb.Tomb
	metricsTomb   tomb.Tomb
	startup       bool
	credentials   *csconfig.ApiCredentialsCfg
	consoleConfig *csconfig.ConsoleConfig
	isPulling     chan bool
	whitelists    *csconfig.CapiWhitelist

	pullBlocklists bool
	pullCommunity  bool
	shareSignals   bool

	TokenSave apiclient.TokenSave
}

// randomDuration returns a duration value between d-delta and d+delta
func randomDuration(d time.Duration, delta time.Duration) time.Duration {
	ret := d + time.Duration(rand.Int63n(int64(2*delta))) - delta
	// ticker interval must be > 0 (nanoseconds)
	if ret <= 0 {
		return 1
	}

	return ret
}

func (a *apic) FetchScenariosListFromDB(ctx context.Context) ([]string, error) {
	scenarios := make([]string, 0)

	machines, err := a.dbClient.ListMachines(ctx)
	if err != nil {
		return nil, fmt.Errorf("while listing machines: %w", err)
	}
	// merge all scenarios together
	for _, v := range machines {
		machineScenarios := strings.Split(v.Scenarios, ",")
		log.Debugf("%d scenarios for machine %d", len(machineScenarios), v.ID)

		for _, sv := range machineScenarios {
			if !slices.Contains(scenarios, sv) && sv != "" {
				scenarios = append(scenarios, sv)
			}
		}
	}

	log.Debugf("Returning list of scenarios : %+v", scenarios)

	return scenarios, nil
}

func decisionsToAPIDecisions(decisions []*models.Decision) models.AddSignalsRequestItemDecisions {
	apiDecisions := models.AddSignalsRequestItemDecisions{}

	for _, decision := range decisions {
		x := &models.AddSignalsRequestItemDecisionsItem{
			Duration: ptr.Of(*decision.Duration),
			ID:       new(int64),
			Origin:   ptr.Of(*decision.Origin),
			Scenario: ptr.Of(*decision.Scenario),
			Scope:    ptr.Of(*decision.Scope),
			// Simulated: *decision.Simulated,
			Type:  ptr.Of(*decision.Type),
			Until: decision.Until,
			Value: ptr.Of(*decision.Value),
			UUID:  decision.UUID,
		}
		*x.ID = decision.ID

		if decision.Simulated != nil {
			x.Simulated = *decision.Simulated
		}

		apiDecisions = append(apiDecisions, x)
	}

	return apiDecisions
}

func alertToSignal(alert *models.Alert, scenarioTrust string, shareContext bool) *models.AddSignalsRequestItem {
	signal := &models.AddSignalsRequestItem{
		Message:         alert.Message,
		Scenario:        alert.Scenario,
		ScenarioHash:    alert.ScenarioHash,
		ScenarioVersion: alert.ScenarioVersion,
		Source: &models.AddSignalsRequestItemSource{
			AsName:    alert.Source.AsName,
			AsNumber:  alert.Source.AsNumber,
			Cn:        alert.Source.Cn,
			IP:        alert.Source.IP,
			Latitude:  alert.Source.Latitude,
			Longitude: alert.Source.Longitude,
			Range:     alert.Source.Range,
			Scope:     alert.Source.Scope,
			Value:     alert.Source.Value,
		},
		StartAt:       alert.StartAt,
		StopAt:        alert.StopAt,
		CreatedAt:     alert.CreatedAt,
		MachineID:     alert.MachineID,
		ScenarioTrust: scenarioTrust,
		Decisions:     decisionsToAPIDecisions(alert.Decisions),
		UUID:          alert.UUID,
	}
	if shareContext {
		signal.Context = make([]*models.AddSignalsRequestItemContextItems0, 0)

		for _, meta := range alert.Meta {
			contextItem := models.AddSignalsRequestItemContextItems0{
				Key:   meta.Key,
				Value: meta.Value,
			}
			signal.Context = append(signal.Context, &contextItem)
		}
	}

	return signal
}

func NewAPIC(ctx context.Context, config *csconfig.OnlineApiClientCfg, dbClient *database.Client, consoleConfig *csconfig.ConsoleConfig, apicWhitelist *csconfig.CapiWhitelist) (*apic, error) {
	var err error

	if apicWhitelist == nil {
		apicWhitelist = &csconfig.CapiWhitelist{}
	}

	ret := &apic{
		AlertsAddChan:             make(chan []*models.Alert),
		dbClient:                  dbClient,
		mu:                        sync.Mutex{},
		startup:                   true,
		credentials:               config.Credentials,
		pullTomb:                  tomb.Tomb{},
		pushTomb:                  tomb.Tomb{},
		metricsTomb:               tomb.Tomb{},
		consoleConfig:             consoleConfig,
		pullInterval:              pullIntervalDefault,
		pullIntervalFirst:         randomDuration(pullIntervalDefault, pullIntervalDelta),
		pushInterval:              pushIntervalDefault,
		pushIntervalFirst:         randomDuration(pushIntervalDefault, pushIntervalDelta),
		metricsInterval:           metricsIntervalDefault,
		metricsIntervalFirst:      randomDuration(metricsIntervalDefault, metricsIntervalDelta),
		usageMetricsInterval:      usageMetricsInterval,
		usageMetricsIntervalFirst: randomDuration(usageMetricsInterval, usageMetricsIntervalDelta),
		isPulling:                 make(chan bool, 1),
		whitelists:                apicWhitelist,
		pullBlocklists:            *config.PullConfig.Blocklists,
		pullCommunity:             *config.PullConfig.Community,
		shareSignals:              *config.Sharing,
	}

	apiURL, err := url.Parse(config.Credentials.URL)
	if err != nil {
		return nil, fmt.Errorf("while parsing '%s': %w", config.Credentials.URL, err)
	}

	papiURL, err := url.Parse(config.Credentials.PapiURL)
	if err != nil {
		return nil, fmt.Errorf("while parsing '%s': %w", config.Credentials.PapiURL, err)
	}

	ret.apiClient, err = apiclient.NewClient(&apiclient.Config{
		MachineID:      config.Credentials.Login,
		Password:       strfmt.Password(config.Credentials.Password),
		URL:            apiURL,
		PapiURL:        papiURL,
		VersionPrefix:  "v3",
		UpdateScenario: ret.FetchScenariosListFromDB,
		TokenSave: func(ctx context.Context, tokenKey string, token string) error {
			return dbClient.SaveAPICToken(ctx, tokenKey, token)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("while creating api client: %w", err)
	}

	err = ret.Authenticate(ctx, config)

	return ret, err
}

// Authenticate ensures the API client is authorized to communicate with the CAPI.
// It attempts to reuse a previously saved JWT token from the database, falling back to
// an authentication request if the token is missing, invalid, or expired.
//
// If a new token is obtained, it is saved back to the database for caching.
func (a *apic) Authenticate(ctx context.Context, config *csconfig.OnlineApiClientCfg) error {
	if token, exp, valid := a.dbClient.LoadAPICToken(ctx, log.StandardLogger()); valid {
		log.Debug("using valid token from DB")

		a.apiClient.GetClient().Transport.(*apiclient.JWTTransport).Token = token
		a.apiClient.GetClient().Transport.(*apiclient.JWTTransport).Expiration = exp

		return nil
	}

	log.Debug("No token found, authenticating")

	scenarios, err := a.FetchScenariosListFromDB(ctx)
	if err != nil {
		return fmt.Errorf("get scenario in db: %w", err)
	}

	password := strfmt.Password(config.Credentials.Password)

	authResp, _, err := a.apiClient.Auth.AuthenticateWatcher(ctx, models.WatcherAuthRequest{
		MachineID: &config.Credentials.Login,
		Password:  &password,
		Scenarios: scenarios,
	})
	if err != nil {
		return fmt.Errorf("authenticate watcher (%s): %w", config.Credentials.Login, err)
	}

	if err = a.apiClient.GetClient().Transport.(*apiclient.JWTTransport).Expiration.UnmarshalText([]byte(authResp.Expire)); err != nil {
		return fmt.Errorf("unable to parse jwt expiration: %w", err)
	}

	a.apiClient.GetClient().Transport.(*apiclient.JWTTransport).Token = authResp.Token

	return a.dbClient.SaveAPICToken(ctx, apiclient.TokenDBField, authResp.Token)
}

// keep track of all alerts in cache and push it to CAPI every PushInterval.
func (a *apic) Push(ctx context.Context) error {
	defer trace.CatchPanic("lapi/pushToAPIC")

	var cache models.AddSignalsRequest

	ticker := time.NewTicker(a.pushIntervalFirst)

	log.Infof("Start push to CrowdSec Central API (interval: %s once, then %s)", a.pushIntervalFirst.Round(time.Second), a.pushInterval)

	for {
		select {
		case <-a.pushTomb.Dying(): // if one apic routine is dying, do we kill the others?
			a.pullTomb.Kill(nil)
			a.metricsTomb.Kill(nil)
			log.Infof("push tomb is dying, sending cache (%d elements) before exiting", len(cache))

			if len(cache) == 0 {
				return nil
			}

			go a.Send(ctx, &cache)

			return nil
		case <-ticker.C:
			ticker.Reset(a.pushInterval)

			if len(cache) > 0 {
				a.mu.Lock()
				cacheCopy := cache
				cache = make(models.AddSignalsRequest, 0)
				a.mu.Unlock()
				log.Infof("Signal push: %d signals to push", len(cacheCopy))

				go a.Send(ctx, &cacheCopy)
			}
		case alerts := <-a.AlertsAddChan:
			var signals []*models.AddSignalsRequestItem

			for _, alert := range alerts {
				if ok := shouldShareAlert(alert, a.consoleConfig, a.shareSignals); ok {
					signals = append(signals, alertToSignal(alert, getScenarioTrustOfAlert(alert), *a.consoleConfig.ShareContext))
				}
			}

			a.mu.Lock()
			cache = append(cache, signals...)
			a.mu.Unlock()
		}
	}
}

func getScenarioTrustOfAlert(alert *models.Alert) string {
	scenarioTrust := "certified"
	if alert.ScenarioHash == nil || *alert.ScenarioHash == "" {
		scenarioTrust = "custom"
	} else if alert.ScenarioVersion == nil || *alert.ScenarioVersion == "" || *alert.ScenarioVersion == "?" {
		scenarioTrust = "tainted"
	}

	if len(alert.Decisions) > 0 {
		if *alert.Decisions[0].Origin == types.CscliOrigin {
			scenarioTrust = "manual"
		}
	}

	return scenarioTrust
}

func shouldShareAlert(alert *models.Alert, consoleConfig *csconfig.ConsoleConfig, shareSignals bool) bool {
	if !shareSignals {
		log.Debugf("sharing signals is disabled")
		return false
	}

	if *alert.Simulated {
		log.Debugf("simulation enabled for alert (id:%d), will not be sent to CAPI", alert.ID)
		return false
	}

	switch scenarioTrust := getScenarioTrustOfAlert(alert); scenarioTrust {
	case "manual":
		if !*consoleConfig.ShareManualDecisions {
			log.Debugf("manual decision generated an alert, doesn't send it to CAPI because options is disabled")
			return false
		}
	case "tainted":
		if !*consoleConfig.ShareTaintedScenarios {
			log.Debugf("tainted scenario generated an alert, doesn't send it to CAPI because options is disabled")
			return false
		}
	case "custom":
		if !*consoleConfig.ShareCustomScenarios {
			log.Debugf("custom scenario generated an alert, doesn't send it to CAPI because options is disabled")
			return false
		}
	}

	return true
}

func (a *apic) sendBatch(ctx context.Context, signals []*models.AddSignalsRequestItem) error {
	ctxBatch, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, _, err := a.apiClient.Signal.Add(ctxBatch, (*models.AddSignalsRequest)(&signals))

	return err
}

func (a *apic) Send(ctx context.Context, cacheOrig *models.AddSignalsRequest) {
	/*we do have a problem with this :
	The apic.Push background routine reads from alertToPush chan.
	This chan is filled by Controller.CreateAlert

	If the chan apic.Send hangs, the alertToPush chan will become full,
	with means that Controller.CreateAlert is going to hang, blocking API worker(s).

	So instead, we prefer to cancel write.

	I don't know enough about gin to tell how much of an issue it can be.
	*/
	var cache []*models.AddSignalsRequestItem = *cacheOrig

	batchSize := 50

	for start := 0; start < len(cache); start += batchSize {
		end := min(start+batchSize, len(cache))

		if err := a.sendBatch(ctx, cache[start:end]); err != nil {
			log.Errorf("sending signal to central API: %s", err)
			return
		}
	}
}

func (a *apic) CAPIPullIsOld(ctx context.Context) (bool, error) {
	/*only pull community blocklist if it's older than 1h30 */
	alerts := a.dbClient.Ent.Alert.Query()
	alerts = alerts.Where(alert.HasDecisionsWith(decision.OriginEQ(database.CapiMachineID)))
	alerts = alerts.Where(alert.CreatedAtGTE(time.Now().UTC().Add(-time.Duration(1*time.Hour + 30*time.Minute)))) //nolint:unconvert

	count, err := alerts.Count(ctx)
	if err != nil {
		return false, fmt.Errorf("while looking for CAPI alert: %w", err)
	}

	if count > 0 {
		log.Info("last CAPI pull is newer than 1h30, skip.")
		return false, nil
	}

	return true, nil
}

func (a *apic) HandleDeletedDecisionsV3(ctx context.Context, deletedDecisions []*modelscapi.GetDecisionsStreamResponseDeletedItem, deleteCounters map[string]map[string]int) (int, error) {
	var nbDeleted int

	for _, decisions := range deletedDecisions {
		scope := decisions.Scope

		for _, decision := range decisions.Decisions {
			filter := map[string][]string{
				"value":  {decision},
				"origin": {types.CAPIOrigin},
			}
			if strings.ToLower(*scope) != "ip" {
				filter["scopes"] = []string{*scope}
			}

			dbCliDel, _, err := a.dbClient.ExpireDecisionsWithFilter(ctx, filter)
			if err != nil {
				return 0, fmt.Errorf("expiring decisions error: %w", err)
			}

			updateCounterForDecision(deleteCounters, ptr.Of(types.CAPIOrigin), nil, dbCliDel)
			nbDeleted += dbCliDel
		}
	}

	return nbDeleted, nil
}

func createAlertsForDecisions(decisions []*models.Decision) []*models.Alert {
	newAlerts := make([]*models.Alert, 0)

	for _, decision := range decisions {
		found := false

		for _, sub := range newAlerts {
			if sub.Source.Scope == nil {
				log.Warningf("nil scope in %+v", sub)
				continue
			}

			if *decision.Origin == types.CAPIOrigin {
				if *sub.Source.Scope == types.CAPIOrigin {
					found = true
					break
				}
			} else if *decision.Origin == types.ListOrigin {
				if *sub.Source.Scope == *decision.Origin {
					if sub.Scenario == nil {
						log.Warningf("nil scenario in %+v", sub)
					}

					if *sub.Scenario == *decision.Scenario {
						found = true
						break
					}
				}
			} else {
				log.Warningf("unknown origin %s : %+v", *decision.Origin, decision)
			}
		}

		if !found {
			log.Debugf("Create entry for origin:%s scenario:%s", *decision.Origin, *decision.Scenario)
			newAlerts = append(newAlerts, createAlertForDecision(decision))
		}
	}

	return newAlerts
}

func createAlertForDecision(decision *models.Decision) *models.Alert {
	var (
		scenario string
		scope    string
	)

	switch *decision.Origin {
	case types.CAPIOrigin:
		scenario = types.CAPIOrigin
		scope = types.CAPIOrigin
	case types.ListOrigin:
		scenario = *decision.Scenario
		scope = types.ListOrigin
	default:
		scenario = ""
		scope = ""

		log.Warningf("unknown origin %s", *decision.Origin)
	}

	return &models.Alert{
		Source: &models.Source{
			Scope: ptr.Of(scope),
			Value: ptr.Of(""),
		},
		Scenario:        ptr.Of(scenario),
		Message:         ptr.Of(""),
		StartAt:         ptr.Of(time.Now().UTC().Format(time.RFC3339)),
		StopAt:          ptr.Of(time.Now().UTC().Format(time.RFC3339)),
		Capacity:        ptr.Of(int32(0)),
		Simulated:       ptr.Of(false),
		EventsCount:     ptr.Of(int32(0)),
		Leakspeed:       ptr.Of(""),
		ScenarioHash:    ptr.Of(""),
		ScenarioVersion: ptr.Of(""),
		MachineID:       database.CapiMachineID,
	}
}

// This function takes in list of parent alerts and decisions and then pairs them up.
func fillAlertsWithDecisions(alerts []*models.Alert, decisions []*models.Decision, addCounters map[string]map[string]int) []*models.Alert {
	for _, decision := range decisions {
		// count and create separate alerts for each list
		updateCounterForDecision(addCounters, decision.Origin, decision.Scenario, 1)

		/*CAPI might send lower case scopes, unify it.*/
		switch strings.ToLower(*decision.Scope) {
		case "ip":
			*decision.Scope = types.Ip
		case "range":
			*decision.Scope = types.Range
		}

		found := false
		// add the individual decisions to the right list
		for idx, alert := range alerts {
			if *decision.Origin == types.CAPIOrigin {
				if *alert.Source.Scope == types.CAPIOrigin {
					alerts[idx].Decisions = append(alerts[idx].Decisions, decision)
					found = true

					break
				}
			} else if *decision.Origin == types.ListOrigin {
				if *alert.Source.Scope == types.ListOrigin && *alert.Scenario == *decision.Scenario {
					alerts[idx].Decisions = append(alerts[idx].Decisions, decision)
					found = true

					break
				}
			} else {
				log.Warningf("unknown origin %s", *decision.Origin)
			}
		}

		if !found {
			log.Warningf("Orphaned decision for %s - %s", *decision.Origin, *decision.Scenario)
		}
	}

	return alerts
}

// we receive a list of decisions and links for blocklist and we need to create a list of alerts :
// one alert for "community blocklist"
// one alert per list we're subscribed to
func (a *apic) PullTop(ctx context.Context, forcePull bool) error {
	var err error

	hasPulledAllowlists := false

	// A mutex with TryLock would be a bit simpler
	// But go does not guarantee that TryLock will be able to acquire the lock even if it is available
	select {
	case a.isPulling <- true:
		defer func() {
			<-a.isPulling
		}()
	default:
		return errors.New("pull already in progress")
	}

	if !forcePull {
		if lastPullIsOld, err := a.CAPIPullIsOld(ctx); err != nil {
			return err
		} else if !lastPullIsOld {
			return nil
		}
	}

	log.Debug("Acquiring lock for pullCAPI")

	err = a.dbClient.AcquirePullCAPILock(ctx)
	if a.dbClient.IsLocked(err) {
		log.Info("PullCAPI is already running, skipping")
		return nil
	}

	/*defer lock release*/
	defer func() {
		log.Debug("Releasing lock for pullCAPI")

		if err := a.dbClient.ReleasePullCAPILock(ctx); err != nil {
			log.Errorf("while releasing lock: %v", err)
		}
	}()

	log.Infof("Starting community-blocklist update")

	log.Debugf("Community pull: %t | Blocklist pull: %t", a.pullCommunity, a.pullBlocklists)

	data, _, err := a.apiClient.Decisions.GetStreamV3(ctx, apiclient.DecisionsStreamOpts{Startup: a.startup, CommunityPull: a.pullCommunity, AdditionalPull: a.pullBlocklists})
	if err != nil {
		return fmt.Errorf("get stream: %w", err)
	}

	a.startup = false
	/*to count additions/deletions across lists*/

	log.Debugf("Received %d new decisions", len(data.New))
	log.Debugf("Received %d deleted decisions", len(data.Deleted))

	if data.Links != nil {
		log.Debugf("Received %d blocklists links", len(data.Links.Blocklists))
		log.Debugf("Received %d allowlists links", len(data.Links.Allowlists))
	}

	addCounters, deleteCounters := makeAddAndDeleteCounters()

	// process deleted decisions
	nbDeleted, err := a.HandleDeletedDecisionsV3(ctx, data.Deleted, deleteCounters)
	if err != nil {
		log.Errorf("could not delete decisions from CAPI: %s", err)
	}

	log.Printf("capi/community-blocklist : %d explicit deletions", nbDeleted)

	// Update allowlists before processing decisions
	if data.Links != nil {
		if len(data.Links.Allowlists) > 0 {
			hasPulledAllowlists = true

			if err := a.UpdateAllowlists(ctx, data.Links.Allowlists, forcePull); err != nil {
				log.Errorf("could not update allowlists from CAPI: %s", err)
			}
		}
	}

	if len(data.New) > 0 {
		// create one alert for community blocklist using the first decision
		decisions := a.apiClient.Decisions.GetDecisionsFromGroups(data.New)
		// apply APIC specific whitelists
		decisions = a.ApplyApicWhitelists(ctx, decisions)

		alert := createAlertForDecision(decisions[0])
		alertsFromCapi := []*models.Alert{alert}
		alertsFromCapi = fillAlertsWithDecisions(alertsFromCapi, decisions, addCounters)

		err = a.SaveAlerts(ctx, alertsFromCapi, addCounters, deleteCounters)
		if err != nil {
			log.Errorf("could not save alert for CAPI pull: %s", err)
		}
	} else {
		if a.pullCommunity {
			log.Info("capi/community-blocklist : received 0 new entries (expected if you just installed crowdsec)")
		} else {
			log.Debug("capi/community-blocklist : community blocklist pull is disabled")
		}
	}

	// update allowlists/blocklists
	if data.Links != nil {
		if len(data.Links.Blocklists) > 0 {
			if err := a.UpdateBlocklists(ctx, data.Links.Blocklists, addCounters, forcePull); err != nil {
				log.Errorf("could not update blocklists from CAPI: %s", err)
			}
		}
	}

	if hasPulledAllowlists {
		deleted, err := a.dbClient.ApplyAllowlistsToExistingDecisions(ctx)
		if err != nil {
			log.Errorf("could not apply allowlists to existing decisions: %s", err)
		}

		if deleted > 0 {
			log.Infof("deleted %d decisions from allowlists", deleted)
		}
	}

	return nil
}

// we receive a link to a blocklist, we pull the content of the blocklist and we create one alert
func (a *apic) PullBlocklist(ctx context.Context, blocklist *modelscapi.BlocklistLink, forcePull bool) error {
	addCounters, _ := makeAddAndDeleteCounters()
	if err := a.UpdateBlocklists(ctx, []*modelscapi.BlocklistLink{blocklist}, addCounters, forcePull); err != nil {
		return fmt.Errorf("while pulling blocklist: %w", err)
	}

	return nil
}

func (a *apic) PullAllowlist(ctx context.Context, allowlist *modelscapi.AllowlistLink, forcePull bool) error {
	if err := a.UpdateAllowlists(ctx, []*modelscapi.AllowlistLink{allowlist}, forcePull); err != nil {
		return fmt.Errorf("while pulling allowlist: %w", err)
	}

	return nil
}

func (a *apic) UpdateAllowlists(ctx context.Context, allowlistsLinks []*modelscapi.AllowlistLink, forcePull bool) error {
	if len(allowlistsLinks) == 0 {
		return nil
	}

	defaultClient, err := apiclient.NewDefaultClient(a.apiClient.BaseURL, "", "", nil)
	if err != nil {
		return fmt.Errorf("while creating default client: %w", err)
	}

	for _, link := range allowlistsLinks {
		if log.IsLevelEnabled(log.TraceLevel) {
			log.Tracef("allowlist body: %+v", spew.Sdump(link))
		}

		if link.Name == nil {
			log.Warningf("allowlist has no name")
			continue
		}

		if link.URL == nil {
			log.Warningf("allowlist %s has no URL", *link.Name)
			continue
		}

		if link.ID == nil {
			log.Warningf("allowlist %s has no ID", *link.Name)
			continue
		}

		description := ""
		if link.Description != nil {
			description = *link.Description
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, *link.URL, http.NoBody)
		if err != nil {
			log.Errorf("while pulling allowlist: %s", err)
			continue
		}

		resp, err := defaultClient.GetClient().Do(req)
		if err != nil {
			log.Errorf("while pulling allowlist: %s", err)
			continue
		}
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		items := make([]*models.AllowlistItem, 0)

		for scanner.Scan() {
			item := scanner.Text()
			j := &models.AllowlistItem{}

			if err := json.Unmarshal([]byte(item), j); err != nil {
				log.Errorf("while unmarshalling allowlist item: %s", err)
				continue
			}

			items = append(items, j)
		}

		list, err := a.dbClient.GetAllowListByID(ctx, *link.ID, false)
		if err != nil {
			if !ent.IsNotFound(err) {
				log.Errorf("while getting allowlist %s: %s", *link.Name, err)
				continue
			}
		}

		if list == nil {
			list, err = a.dbClient.CreateAllowList(ctx, *link.Name, description, *link.ID, true)
			if err != nil {
				log.Errorf("while creating allowlist %s: %s", *link.Name, err)
				continue
			}
		}

		added, err := a.dbClient.ReplaceAllowlist(ctx, list, items, true)
		if err != nil {
			log.Errorf("while replacing allowlist %s: %s", *link.Name, err)
			continue
		}

		log.Infof("added %d values to allowlist %s", added, list.Name)

		if list.Name != *link.Name || list.Description != description {
			err = a.dbClient.UpdateAllowlistMeta(ctx, *link.ID, *link.Name, description)
			if err != nil {
				log.Errorf("while updating allowlist meta %s: %s", *link.Name, err)
				continue
			}
		}

		log.Infof("Allowlist %s updated", *link.Name)
	}

	return nil
}

// if decisions is whitelisted: return representation of the whitelist ip or cidr
// if not whitelisted: empty string
func (a *apic) whitelistedBy(decision *models.Decision, additionalIPs []netip.Addr, additionalRanges []netip.Prefix) string {
	if decision.Value == nil {
		return ""
	}

	ipval, err := netip.ParseAddr(*decision.Value)
	if err != nil {
		return ""
	}

	for _, cidr := range a.whitelists.Cidrs {
		if cidr.Contains(ipval) {
			return cidr.String()
		}
	}

	for _, ip := range a.whitelists.Ips {
		if ip == ipval {
			return ip.String()
		}
	}

	for _, ip := range additionalIPs {
		if ip == ipval {
			return ip.String()
		}
	}

	for _, cidr := range additionalRanges {
		if cidr.Contains(ipval) {
			return cidr.String()
		}
	}

	return ""
}

func (a *apic) ApplyApicWhitelists(ctx context.Context, decisions []*models.Decision) []*models.Decision {
	allowlisted_ips, allowlisted_cidrs, err := a.dbClient.GetAllowlistsContentForAPIC(ctx)
	if err != nil {
		log.Errorf("while getting allowlists content: %s", err)
	}

	if a.whitelists != nil && (len(a.whitelists.Cidrs) > 0 || len(a.whitelists.Ips) > 0) {
		log.Warn("capi_whitelists_path is deprecated, please use centralized allowlists instead. See https://docs.crowdsec.net/docs/next/local_api/centralized_allowlists.")
	}

	if (a.whitelists == nil || len(a.whitelists.Cidrs) == 0 && len(a.whitelists.Ips) == 0) && len(allowlisted_ips) == 0 && len(allowlisted_cidrs) == 0 {
		return decisions
	}
	// deal with CAPI whitelists for fire. We want to avoid having a second list, so we shrink in place
	outIdx := 0

	for _, decision := range decisions {
		whitelister := a.whitelistedBy(decision, allowlisted_ips, allowlisted_cidrs)
		if whitelister != "" {
			log.Infof("%s from %s is whitelisted by %s", *decision.Value, *decision.Scenario, whitelister)
			continue
		}

		decisions[outIdx] = decision
		outIdx++
	}
	// shrink the list, those are deleted items
	return decisions[:outIdx]
}

func (a *apic) SaveAlerts(ctx context.Context, alertsFromCapi []*models.Alert, addCounters map[string]map[string]int, deleteCounters map[string]map[string]int) error {
	for _, alert := range alertsFromCapi {
		setAlertScenario(alert, addCounters, deleteCounters)
		log.Debugf("%s has %d decisions", *alert.Source.Scope, len(alert.Decisions))

		if a.dbClient.Type == "sqlite" && (a.dbClient.WalMode == nil || !*a.dbClient.WalMode) {
			log.Warningf("sqlite is not using WAL mode, LAPI might become unresponsive when inserting the community blocklist")
		}

		alertID, inserted, deleted, err := a.dbClient.UpdateCommunityBlocklist(ctx, alert)
		if err != nil {
			return fmt.Errorf("while saving alert from %s: %w", *alert.Source.Scope, err)
		}

		log.Printf("%s : added %d entries, deleted %d entries (alert:%d)", *alert.Source.Scope, inserted, deleted, alertID)
	}

	return nil
}

func (a *apic) ShouldForcePullBlocklist(ctx context.Context, blocklist *modelscapi.BlocklistLink) (bool, error) {
	// we should force pull if the blocklist decisions are about to expire or there's no decision in the db
	alertQuery := a.dbClient.Ent.Alert.Query()
	alertQuery.Where(alert.SourceScopeEQ(fmt.Sprintf("%s:%s", types.ListOrigin, *blocklist.Name)))
	alertQuery.Order(ent.Desc(alert.FieldCreatedAt))

	alertInstance, err := alertQuery.First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			log.Debugf("no alert found for %s, force refresh", *blocklist.Name)
			return true, nil
		}

		return false, fmt.Errorf("while getting alert: %w", err)
	}

	decisionQuery := a.dbClient.Ent.Decision.Query()
	decisionQuery.Where(decision.HasOwnerWith(alert.IDEQ(alertInstance.ID)))

	firstDecision, err := decisionQuery.First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			log.Debugf("no decision found for %s, force refresh", *blocklist.Name)
			return true, nil
		}

		return false, fmt.Errorf("while getting decision: %w", err)
	}

	if firstDecision == nil || firstDecision.Until == nil || firstDecision.Until.Sub(time.Now().UTC()) < (a.pullInterval+15*time.Minute) {
		log.Debugf("at least one decision found for %s, expire soon, force refresh", *blocklist.Name)
		return true, nil
	}

	return false, nil
}

func (a *apic) updateBlocklist(ctx context.Context, client *apiclient.ApiClient, blocklist *modelscapi.BlocklistLink, addCounters map[string]map[string]int, forcePull bool) error {
	if blocklist.Scope == nil {
		log.Warningf("blocklist has no scope")
		return nil
	}

	if blocklist.Duration == nil {
		log.Warningf("blocklist has no duration")
		return nil
	}

	if !forcePull {
		_forcePull, err := a.ShouldForcePullBlocklist(ctx, blocklist)
		if err != nil {
			return fmt.Errorf("while checking if we should force pull blocklist %s: %w", *blocklist.Name, err)
		}

		forcePull = _forcePull
	}

	blocklistConfigItemName := fmt.Sprintf("blocklist:%s:last_pull", *blocklist.Name)

	var (
		lastPullTimestamp string
		err               error
	)

	if !forcePull {
		lastPullTimestamp, err = a.dbClient.GetConfigItem(ctx, blocklistConfigItemName)
		if err != nil {
			return fmt.Errorf("while getting last pull timestamp for blocklist %s: %w", *blocklist.Name, err)
		}
	}

	decisions, hasChanged, err := client.Decisions.GetDecisionsFromBlocklist(ctx, blocklist, lastPullTimestamp)
	if err != nil {
		return fmt.Errorf("while getting decisions from blocklist %s: %w", *blocklist.Name, err)
	}

	if !hasChanged {
		if lastPullTimestamp == "" {
			log.Infof("blocklist %s hasn't been modified or there was an error reading it, skipping", *blocklist.Name)
		} else {
			log.Infof("blocklist %s hasn't been modified since %s, skipping", *blocklist.Name, lastPullTimestamp)
		}

		return nil
	}

	err = a.dbClient.SetConfigItem(ctx, blocklistConfigItemName, time.Now().UTC().Format(http.TimeFormat))
	if err != nil {
		return fmt.Errorf("while setting last pull timestamp for blocklist %s: %w", *blocklist.Name, err)
	}

	if len(decisions) == 0 {
		log.Infof("blocklist %s has no decisions", *blocklist.Name)
		return nil
	}
	// apply APIC specific whitelists
	decisions = a.ApplyApicWhitelists(ctx, decisions)
	alert := createAlertForDecision(decisions[0])
	alertsFromCapi := []*models.Alert{alert}
	alertsFromCapi = fillAlertsWithDecisions(alertsFromCapi, decisions, addCounters)

	err = a.SaveAlerts(ctx, alertsFromCapi, addCounters, nil)
	if err != nil {
		return fmt.Errorf("while saving alert from blocklist %s: %w", *blocklist.Name, err)
	}

	return nil
}

func (a *apic) UpdateBlocklists(ctx context.Context, blocklists []*modelscapi.BlocklistLink, addCounters map[string]map[string]int, forcePull bool) error {
	if len(blocklists) == 0 {
		return nil
	}

	// we must use a different http client than apiClient's because the transport of apiClient is jwtTransport or here we have signed apis that are incompatibles
	// we can use the same baseUrl as the urls are absolute and the parse will take care of it
	defaultClient, err := apiclient.NewDefaultClient(a.apiClient.BaseURL, "", "", nil)
	if err != nil {
		return fmt.Errorf("while creating default client: %w", err)
	}

	for _, blocklist := range blocklists {
		if err := a.updateBlocklist(ctx, defaultClient, blocklist, addCounters, forcePull); err != nil {
			return err
		}
	}

	return nil
}

func setAlertScenario(alert *models.Alert, addCounters map[string]map[string]int, deleteCounters map[string]map[string]int) {
	switch *alert.Source.Scope {
	case types.CAPIOrigin:
		*alert.Source.Scope = types.CommunityBlocklistPullSourceScope
		alert.Scenario = ptr.Of(fmt.Sprintf("update : +%d/-%d IPs",
			addCounters[types.CAPIOrigin]["all"],
			deleteCounters[types.CAPIOrigin]["all"]))
	case types.ListOrigin:
		*alert.Source.Scope = fmt.Sprintf("%s:%s", types.ListOrigin, *alert.Scenario)
		alert.Scenario = ptr.Of(fmt.Sprintf("update : +%d/-%d IPs",
			addCounters[types.ListOrigin][*alert.Scenario],
			deleteCounters[types.ListOrigin][*alert.Scenario]))
	}
}

func (a *apic) Pull(ctx context.Context) error {
	defer trace.CatchPanic("lapi/pullFromAPIC")

	toldOnce := false

	for {
		scenario, err := a.FetchScenariosListFromDB(ctx)
		if err != nil {
			log.Errorf("unable to fetch scenarios from db: %s", err)
		}

		if len(scenario) > 0 {
			break
		}

		if !toldOnce {
			log.Warning("scenario list is empty, will not pull yet")

			toldOnce = true
		}

		time.Sleep(1 * time.Second)
	}

	if err := a.PullTop(ctx, false); err != nil {
		log.Errorf("capi pull top: %s", err)
	}

	log.Infof("Start pull from CrowdSec Central API (interval: %s once, then %s)", a.pullIntervalFirst.Round(time.Second), a.pullInterval)
	ticker := time.NewTicker(a.pullIntervalFirst)

	for {
		select {
		case <-ticker.C:
			ticker.Reset(a.pullInterval)

			if err := a.PullTop(ctx, false); err != nil {
				log.Errorf("capi pull top: %s", err)
				continue
			}
		case <-a.pullTomb.Dying(): // if one apic routine is dying, do we kill the others?
			a.metricsTomb.Kill(nil)
			a.pushTomb.Kill(nil)

			return nil
		}
	}
}

func (a *apic) Shutdown() {
	a.pushTomb.Kill(nil)
	a.pullTomb.Kill(nil)
	a.metricsTomb.Kill(nil)
}

func makeAddAndDeleteCounters() (map[string]map[string]int, map[string]map[string]int) {
	addCounters := make(map[string]map[string]int)
	addCounters[types.CAPIOrigin] = make(map[string]int)
	addCounters[types.ListOrigin] = make(map[string]int)

	deleteCounters := make(map[string]map[string]int)
	deleteCounters[types.CAPIOrigin] = make(map[string]int)
	deleteCounters[types.ListOrigin] = make(map[string]int)

	return addCounters, deleteCounters
}

func updateCounterForDecision(counter map[string]map[string]int, origin *string, scenario *string, totalDecisions int) {
	switch *origin {
	case types.CAPIOrigin:
		counter[*origin]["all"] += totalDecisions
	case types.ListOrigin:
		counter[*origin][*scenario] += totalDecisions
	default:
		log.Warningf("Unknown origin %s", *origin)
	}
}
