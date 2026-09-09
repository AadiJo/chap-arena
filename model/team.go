// Copyright 2014 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)
//
// Describes the radio identity of the team occupying an alliance station. Chap Arena doesn't keep a
// team database; team numbers are typed directly into the station configuration page, and every
// station shares the WPA key configured on the settings page.

package model

type Team struct {
	Id     int `db:"id,manual"`
	WpaKey string
}
