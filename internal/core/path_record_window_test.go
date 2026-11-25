package core

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/stretchr/testify/require"
)

// httpStatus performs a JSON API request and returns the status and body.
func httpStatus(t *testing.T, method, ur string, in any) (int, string) {
	t.Helper()

	var buf *bytes.Buffer
	if in != nil {
		byts, err := json.Marshal(in)
		require.NoError(t, err)
		buf = bytes.NewBuffer(byts)
	}

	req, err := http.NewRequest(method, ur, buf)
	require.NoError(t, err)

	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()

	var body bytes.Buffer
	_, err = body.ReadFrom(res.Body)
	require.NoError(t, err)

	return res.StatusCode, body.String()
}

func windowConf(dir, window string) string {
	return "api: yes\n" +
		"record: yes\n" +
		"recordPath: " + filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f") + "\n" +
		"paths:\n" +
		"  mystream:\n" +
		"    recordWindow: \"" + window + "\"\n"
}

// currentOpenWindow returns a HH:MM-HH:MM UTC window covering the present.
func currentOpenWindow(t *testing.T) string {
	t.Helper()
	now := time.Now().UTC()
	return now.Add(-time.Hour).Format("15:04") + "-" + now.Add(time.Hour).Format("15:04")
}

// currentClosedWindow returns a HH:MM-HH:MM UTC window that does not cover now.
func currentClosedWindow(t *testing.T) string {
	t.Helper()
	now := time.Now().UTC()
	return now.Add(time.Hour).Format("15:04") + "-" + now.Add(2*time.Hour).Format("15:04")
}

// TestPathRecordWindowConfig verifies that invalid windows prevent startup and
// are rejected by the hot-reload API with a clear error, while valid windows
// are accepted.
func TestPathRecordWindowConfig(t *testing.T) {
	dir := t.TempDir()

	// invalid window at startup: the instance must fail to start
	_, ok := newInstance(t, windowConf(dir, "25:00-10:00"))
	require.False(t, ok, "an invalid recordWindow must prevent startup")

	// valid instance
	p, ok := newInstance(t, windowConf(dir, "09:00-17:00"))
	require.True(t, ok)
	defer p.Close()

	// PATCH an invalid window: 400 and a clear message
	status, body := httpStatus(t, http.MethodPatch,
		"http://localhost:9997/v3/config/paths/patch/mystream",
		map[string]any{"recordWindow": "09:00-09:00"})
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, body, "start and end must differ")

	// a valid cross-midnight window is accepted and hot-reloaded
	status, _ = httpStatus(t, http.MethodPatch,
		"http://localhost:9997/v3/config/paths/patch/mystream",
		map[string]any{"recordWindow": "22:00-06:00"})
	require.Equal(t, http.StatusOK, status)
}

// TestPathRecordWindowOffline verifies that when the publisher goes offline the
// segment being written is fully finalized and no further data is written.
func TestPathRecordWindowOffline(t *testing.T) {
	dir := t.TempDir()
	recDir := filepath.Join(dir, "mystream")

	p, ok := newInstance(t, windowConf(dir, currentOpenWindow(t)))
	require.True(t, ok)
	defer p.Close()

	source, write := publishH264(t, "mystream", 90000)

	for range 10 {
		require.NoError(t, write())
	}

	require.Eventually(t, func() bool {
		n, _ := recordedFiles(t, recDir)
		return n >= 1
	}, 10*time.Second, 100*time.Millisecond, "no segment recorded inside the open window")

	// publisher disconnects: the segment must be finalized
	source.Close()

	require.Eventually(t, func() bool {
		n, size := recordedFiles(t, recDir)
		return n >= 1 && size > 0
	}, 5*time.Second, 100*time.Millisecond)

	n1, size1 := recordedFiles(t, recDir)
	require.Positive(t, n1)

	time.Sleep(1500 * time.Millisecond)
	n2, size2 := recordedFiles(t, recDir)
	require.Equal(t, n1, n2, "recording continued after the stream went offline")
	require.Equal(t, size1, size2, "segment kept growing after the stream went offline")
}

// TestPathRecordWindowHotReload verifies that changing the window through the
// API starts and stops recording on a live stream without reconnecting the
// publisher.
func TestPathRecordWindowHotReload(t *testing.T) {
	dir := t.TempDir()
	recDir := filepath.Join(dir, "mystream")
	closed := currentClosedWindow(t)

	p, ok := newInstance(t, windowConf(dir, closed))
	require.True(t, ok)
	defer p.Close()

	// a single publisher stays connected across every reload
	source, write := publishH264(t, "mystream", 90000)
	defer source.Close()

	// window closed: bursts must not produce any file
	for range 6 {
		require.NoError(t, write())
	}
	time.Sleep(1 * time.Second)
	n, _ := recordedFiles(t, recDir)
	require.Zero(t, n, "a segment was created while the window was closed")

	// open a window covering the current time
	status, _ := httpStatus(t, http.MethodPatch,
		"http://localhost:9997/v3/config/paths/patch/mystream",
		map[string]any{"recordWindow": currentOpenWindow(t)})
	require.Equal(t, http.StatusOK, status)

	// keep writing bursts until the recorder has restarted under the new window
	require.Eventually(t, func() bool {
		for range 4 {
			if err := write(); err != nil {
				return false
			}
		}
		n, _ := recordedFiles(t, recDir)
		return n >= 1
	}, 10*time.Second, 200*time.Millisecond,
		"recording did not start after the window was opened by reload")

	// close the window again through reload; the segment must be finalized and
	// later packets must not create or grow anything
	status, _ = httpStatus(t, http.MethodPatch,
		"http://localhost:9997/v3/config/paths/patch/mystream",
		map[string]any{"recordWindow": closed})
	require.Equal(t, http.StatusOK, status)

	require.Eventually(t, func() bool {
		n1, s1 := recordedFiles(t, recDir)
		if n1 == 0 || s1 == 0 {
			return false
		}
		time.Sleep(300 * time.Millisecond)
		n2, s2 := recordedFiles(t, recDir)
		return n1 == n2 && s1 == s2
	}, 5*time.Second, 200*time.Millisecond, "segment was not finalized after window close reload")

	for range 6 {
		require.NoError(t, write())
	}
	time.Sleep(1500 * time.Millisecond)
	n1, size1 := recordedFiles(t, recDir)
	time.Sleep(1500 * time.Millisecond)
	n2, size2 := recordedFiles(t, recDir)
	require.Equal(t, n1, n2, "a new segment appeared after the window was closed by reload")
	require.Equal(t, size1, size2, "the segment kept growing after the window was closed by reload")
}

// TestPathRecordWindowRestart verifies that shutting the program down
// finalizes the current segment and that a restart with the same window keeps
// the existing recording and records new media while the window is open.
func TestPathRecordWindowRestart(t *testing.T) {
	dir := t.TempDir()
	recDir := filepath.Join(dir, "mystream")
	cnf := windowConf(dir, currentOpenWindow(t))

	p1, ok := newInstance(t, cnf)
	require.True(t, ok)

	source, write := publishH264(t, "mystream", 90000)
	for range 10 {
		require.NoError(t, write())
	}

	require.Eventually(t, func() bool {
		n, _ := recordedFiles(t, recDir)
		return n >= 1
	}, 10*time.Second, 100*time.Millisecond)

	source.Close()

	// shutdown finalizes every segment
	p1.Close()

	entries, err := os.ReadDir(recDir)
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	firstPath := filepath.Join(recDir, entries[0].Name())
	firstInfo, err := os.Stat(firstPath)
	require.NoError(t, err)
	require.Positive(t, firstInfo.Size())

	// the finalized fMP4 segment must be parseable after shutdown
	byts, err := os.ReadFile(firstPath)
	require.NoError(t, err)
	var parts fmp4.Parts
	require.NoError(t, parts.Unmarshal(byts))

	// restart the program with the same configuration
	p2, ok := newInstance(t, cnf)
	require.True(t, ok)
	defer p2.Close()

	source2, write2 := publishH264(t, "mystream", 90000)
	defer source2.Close()

	for range 10 {
		require.NoError(t, write2())
	}

	require.Eventually(t, func() bool {
		n, _ := recordedFiles(t, recDir)
		return n >= len(entries)+1
	}, 10*time.Second, 100*time.Millisecond, "restart did not produce a new segment")

	// the segment from the previous run must be untouched
	info, err := os.Stat(firstPath)
	require.NoError(t, err)
	require.Equal(t, firstInfo.Size(), info.Size(), "segment from previous run was modified after restart")
}
