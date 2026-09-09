// Copyright 2014 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)

package web

import (
	"github.com/AadiJo/chap-arena/field"
	"github.com/stretchr/testify/assert"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIndexShowsStations(t *testing.T) {
	web := setupTestWeb(t)

	recorder := web.getHttpResponse("/")
	assert.Equal(t, 200, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "Untitled Event")
	for _, name := range field.StationNames {
		assert.Contains(t, recorder.Body.String(), name)
	}
}

func (web *Web) getHttpResponse(path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", path, nil)
	web.newHandler().ServeHTTP(recorder, req)
	return recorder
}

func (web *Web) getHttpResponseWithHeaders(path string, headers map[string]string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", path, nil)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	web.newHandler().ServeHTTP(recorder, req)
	return recorder
}

func (web *Web) postHttpResponse(path string, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; param=value")
	web.newHandler().ServeHTTP(recorder, req)
	return recorder
}

func setupTestWeb(t *testing.T) *Web {
	return NewWeb(field.SetupTestField(t))
}
