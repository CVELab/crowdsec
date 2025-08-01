package journalctlacquisition

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/tomb.v2"

	"github.com/crowdsecurity/go-cs-lib/cstest"

	"github.com/crowdsecurity/crowdsec/pkg/metrics"
	"github.com/crowdsecurity/crowdsec/pkg/types"
)

func TestBadConfiguration(t *testing.T) {
	cstest.SkipOnWindows(t)

	tests := []struct {
		config      string
		expectedErr string
	}{
		{
			config:      `foobar: asd.log`,
			expectedErr: `cannot parse JournalCtlSource configuration: [1:1] unknown field "foobar"`,
		},
		{
			config: `
mode: tail
source: journalctl`,
			expectedErr: "journalctl_filter is required",
		},
		{
			config: `
mode: cat
source: journalctl
journalctl_filter:
 - _UID=42`,
			expectedErr: "",
		},
	}

	subLogger := log.WithField("type", "journalctl")

	for _, tc := range tests {
		t.Run(tc.config, func(t *testing.T) {
			f := JournalCtlSource{}
			err := f.Configure([]byte(tc.config), subLogger, metrics.AcquisitionMetricsLevelNone)
			cstest.RequireErrorContains(t, err, tc.expectedErr)
		})
	}
}

func TestConfigureDSN(t *testing.T) {
	cstest.SkipOnWindows(t)

	tests := []struct {
		dsn         string
		expectedErr string
	}{
		{
			dsn:         "asd://",
			expectedErr: "invalid DSN asd:// for journalctl source, must start with journalctl://",
		},
		{
			dsn:         "journalctl://",
			expectedErr: "empty journalctl:// DSN",
		},
		{
			dsn:         "journalctl://foobar=42",
			expectedErr: "unsupported key foobar in journalctl DSN",
		},
		{
			dsn:         "journalctl://filters=%ZZ",
			expectedErr: "could not parse journalctl DSN: invalid URL escape \"%ZZ\"",
		},
		{
			dsn:         "journalctl://filters=_UID=42?log_level=warn",
			expectedErr: "",
		},
		{
			dsn:         "journalctl://filters=_UID=1000&log_level=foobar",
			expectedErr: "unknown level foobar: not a valid logrus Level:",
		},
		{
			dsn:         "journalctl://filters=_UID=1000&log_level=warn&since=yesterday",
			expectedErr: "",
		},
	}

	subLogger := log.WithField("type", "journalctl")

	for _, test := range tests {
		f := JournalCtlSource{}
		err := f.ConfigureByDSN(test.dsn, map[string]string{"type": "testtype"}, subLogger, "")
		cstest.AssertErrorContains(t, err, test.expectedErr)
	}
}

func TestOneShot(t *testing.T) {
	cstest.SkipOnWindows(t)

	ctx := t.Context()

	tests := []struct {
		config         string
		expectedErr    string
		expectedOutput string
		expectedLines  int
		logLevel       log.Level
	}{
		{
			config: `
source: journalctl
mode: cat
journalctl_filter:
 - "-_UID=42"`,
			expectedErr:    "",
			expectedOutput: "journalctl: invalid option",
			logLevel:       log.WarnLevel,
			expectedLines:  0,
		},
		{
			config: `
source: journalctl
mode: cat
journalctl_filter:
 - _SYSTEMD_UNIT=ssh.service`,
			expectedErr:    "",
			expectedOutput: "",
			logLevel:       log.WarnLevel,
			expectedLines:  14,
		},
	}
	for _, ts := range tests {
		var (
			logger    *log.Logger
			subLogger *log.Entry
			hook      *test.Hook
		)

		if ts.expectedOutput != "" {
			logger, hook = test.NewNullLogger()
			logger.SetLevel(ts.logLevel)
			subLogger = logger.WithField("type", "journalctl")
		} else {
			subLogger = log.WithField("type", "journalctl")
		}

		tomb := tomb.Tomb{}
		out := make(chan types.Event, 100)
		j := JournalCtlSource{}

		err := j.Configure([]byte(ts.config), subLogger, metrics.AcquisitionMetricsLevelNone)
		if err != nil {
			t.Fatalf("Unexpected error : %s", err)
		}

		err = j.OneShotAcquisition(ctx, out, &tomb)
		cstest.AssertErrorContains(t, err, ts.expectedErr)

		if err != nil {
			continue
		}

		if ts.expectedLines != 0 {
			assert.Len(t, out, ts.expectedLines)
		}

		if ts.expectedOutput != "" {
			if hook.LastEntry() == nil {
				t.Fatalf("Expected log output '%s' but got nothing !", ts.expectedOutput)
			}

			assert.Contains(t, hook.LastEntry().Message, ts.expectedOutput)
			hook.Reset()
		}
	}
}

func TestStreaming(t *testing.T) {
	cstest.SkipOnWindows(t)

	ctx := t.Context()

	tests := []struct {
		config         string
		expectedErr    string
		expectedOutput string
		expectedLines  int
		logLevel       log.Level
	}{
		{
			config: `
source: journalctl
mode: cat
journalctl_filter:
 - _SYSTEMD_UNIT=ssh.service`,
			expectedErr:    "",
			expectedOutput: "",
			logLevel:       log.WarnLevel,
			expectedLines:  14,
		},
	}
	for _, ts := range tests {
		var (
			logger    *log.Logger
			subLogger *log.Entry
			hook      *test.Hook
		)

		if ts.expectedOutput != "" {
			logger, hook = test.NewNullLogger()
			logger.SetLevel(ts.logLevel)
			subLogger = logger.WithField("type", "journalctl")
		} else {
			subLogger = log.WithField("type", "journalctl")
		}

		tomb := tomb.Tomb{}
		out := make(chan types.Event)
		j := JournalCtlSource{}

		err := j.Configure([]byte(ts.config), subLogger, metrics.AcquisitionMetricsLevelNone)
		if err != nil {
			t.Fatalf("Unexpected error : %s", err)
		}

		actualLines := 0

		if ts.expectedLines != 0 {
			go func() {
			READLOOP:
				for {
					select {
					case <-out:
						actualLines++
					case <-time.After(1 * time.Second):
						break READLOOP
					}
				}
			}()
		}

		err = j.StreamingAcquisition(ctx, out, &tomb)
		cstest.AssertErrorContains(t, err, ts.expectedErr)

		if err != nil {
			continue
		}

		if ts.expectedLines != 0 {
			time.Sleep(1 * time.Second)
			assert.Equal(t, ts.expectedLines, actualLines)
		}

		tomb.Kill(nil)
		err = tomb.Wait()
		require.NoError(t, err)

		output, _ := exec.Command("pgrep", "-x", "journalctl").CombinedOutput()
		if len(output) != 0 {
			t.Fatalf("Found a journalctl process after killing the tomb !")
		}

		if ts.expectedOutput != "" {
			if hook.LastEntry() == nil {
				t.Fatalf("Expected log output '%s' but got nothing !", ts.expectedOutput)
			}

			assert.Contains(t, hook.LastEntry().Message, ts.expectedOutput)
			hook.Reset()
		}
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("USE_SYSTEM_JOURNALCTL") == "" {
		fullPath, _ := filepath.Abs("./testdata")
		os.Setenv("PATH", fullPath+":"+os.Getenv("PATH"))
	}

	os.Exit(m.Run())
}
