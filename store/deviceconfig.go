// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package store

import (
	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waWa6"
)

// SetDeviceFingerprintIOS configures the client to impersonate an iOS device.
// This is required for ViewOnce message interception to work.
// Must be called before any WhatsApp connection is established.
func SetDeviceFingerprintIOS() {
	// BaseClientPayload: change from WEB to iOS
	BaseClientPayload.UserAgent.Platform = waWa6.ClientPayload_UserAgent_IOS.Enum()
	BaseClientPayload.UserAgent.OsVersion = proto.String("18.2")
	BaseClientPayload.UserAgent.Manufacturer = proto.String("Apple")
	BaseClientPayload.UserAgent.Device = proto.String("iPhone")
	BaseClientPayload.UserAgent.OsBuildNumber = proto.String("22C161")
	BaseClientPayload.UserAgent.DeviceType = waWa6.ClientPayload_UserAgent_PHONE.Enum()

	// Remove WebInfo (only present on web clients, not on iOS)
	BaseClientPayload.WebInfo = nil

	// DeviceProps: change to iOS
	DeviceProps.Os = proto.String("iOS")
	DeviceProps.Version.Primary = proto.Uint32(18)
	DeviceProps.Version.Secondary = proto.Uint32(2)
	DeviceProps.Version.Tertiary = proto.Uint32(0)
	DeviceProps.PlatformType = waCompanionReg.DeviceProps_IOS_PHONE.Enum()
	DeviceProps.HistorySyncConfig.SupportCallLogHistory = proto.Bool(false)
}
