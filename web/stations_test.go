// Copyright 2026 Advait Johari. All Rights Reserved.

package web

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestStationsApply(t *testing.T) {
	web := setupTestWeb(t)

	recorder := web.postHttpResponse("/apply", "red1=254&red2=&red3=1678&blue1=&blue2=&blue3=604")
	assert.Equal(t, 303, recorder.Code)
	assert.Equal(t, [6]int{254, 0, 1678, 0, 0, 604}, web.field.Settings.StationTeamIds())

	// The saved assignment comes back on the page, and empty stations render as blank rather than 0.
	recorder = web.getHttpResponse("/")
	assert.Equal(t, 200, recorder.Code)
	body := recorder.Body.String()
	assert.Contains(t, body, `value="254"`)
	assert.Contains(t, body, `value="1678"`)
	assert.Contains(t, body, `value="604"`)
	assert.NotContains(t, body, `value="0"`)
}

func TestStationsApplyInvalidInput(t *testing.T) {
	web := setupTestWeb(t)

	recorder := web.postHttpResponse("/apply", "red1=banana")
	assert.Equal(t, 200, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `Red 1: &#34;banana&#34; is not a team number.`)

	recorder = web.postHttpResponse("/apply", "red1=254&blue3=254")
	assert.Equal(t, 200, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "team 254 is assigned to more than one station")

	assert.Equal(t, [6]int{}, web.field.Settings.StationTeamIds())
}

func TestSettingsRejectsBadWpaKey(t *testing.T) {
	web := setupTestWeb(t)

	recorder := web.postHttpResponse("/setup/settings", "apChannel=36&teamWpaKey=short")
	assert.Equal(t, 200, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "must be 8-63 characters; got 5")
	assert.Equal(t, "", web.field.Settings.TeamWpaKey)

	recorder = web.postHttpResponse("/setup/settings", "apChannel=36&teamWpaKey=chapsrule&apAddress=10.0.100.2")
	assert.Equal(t, 200, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "Settings saved.")
	assert.Equal(t, "chapsrule", web.field.Settings.TeamWpaKey)
	assert.Equal(t, "10.0.100.2", web.field.Settings.ApAddress)
}
