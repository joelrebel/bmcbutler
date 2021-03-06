// Copyright © 2018 Joel Rebello <joel.rebello@booking.com>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package butler

import (
	"fmt"
	"github.com/bmc-toolbox/bmcbutler/asset"
	"github.com/bmc-toolbox/bmclib/cfgresources"
	"github.com/bmc-toolbox/bmclib/devices"
	"github.com/bmc-toolbox/bmclib/discover"
	"github.com/bmc-toolbox/bmclib/logging"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"sync"
)

func init() {
	//set formatter for bmclib logger
	logging.SetFormatter(&logrus.TextFormatter{})
}

type ButlerMsg struct {
	Assets []asset.Asset
	Config *cfgresources.ResourcesConfig
}

type Butler struct {
	Log            *logrus.Logger
	SpawnCount     int
	SyncWG         sync.WaitGroup
	Channel        <-chan ButlerMsg
	IgnoreLocation bool
}

// spawn a pool of butlers
func (b *Butler) Spawn() {

	log := b.Log
	component := "butler-spawn"

	for i := 0; i <= b.SpawnCount; i++ {
		b.SyncWG.Add(1)
		go b.butler(i)
	}

	log.WithFields(logrus.Fields{
		"component": component,
		"count":     b.SpawnCount,
	}).Info("Spawned butlers.")

	//runtime.Goexit()

}

func (b *Butler) Wait() {
	b.SyncWG.Wait()
}

func myLocation(location string) bool {
	myLocations := viper.GetStringSlice("locations")
	for _, l := range myLocations {
		if l == location {
			return true
		}
	}

	return false
}

// butler recieves config, assets over channel
// iterate over assets and apply config
func (b *Butler) butler(id int) {

	log := b.Log
	component := "butler-worker"
	defer b.SyncWG.Done()

	for {
		msg, ok := <-b.Channel
		if !ok {
			log.WithFields(logrus.Fields{
				"component": component,
				"butler-id": id,
			}).Debug("butler msg channel was closed, goodbye.")
			return
		}

		for _, asset := range msg.Assets {

			//if asset has no IPAddress, we can't do anything about it
			if asset.IpAddress == "" {
				log.WithFields(logrus.Fields{
					"Asset": asset,
				}).Warn("Ignored asset since no IpAddress was set.")
				continue
			}

			//if asset has a location defined, we may want to filter it
			if asset.Location != "" {
				if !myLocation(asset.Location) && !b.IgnoreLocation {
					log.WithFields(logrus.Fields{
						"Asset": asset,
					}).Info("Ignored asset since location did not match.")
					continue
				}
			}

			log.WithFields(logrus.Fields{
				"component": component,
				"butler-id": id,
				"AssetType": asset.Type,
				"IP":        asset.IpAddress,
				"Vendor":    asset.Vendor,
				"Serial":    asset.Serial,
				"Location":  asset.Location,
			}).Info("Configuring asset..")

			//this asset needs to be setup
			if asset.Setup == true {
				b.setupAsset(id, msg.Config, &asset)
			}

			b.applyConfig(id, msg.Config, &asset)

		}
	}
}

// connects to the asset and returns the bmc connection
func (b *Butler) connectAsset(asset *asset.Asset, useDefaultLogin bool) (bmcConnection interface{}, err error) {

	var bmcUser, bmcPassword string
	log := b.Log
	component := "butler-connect-asset"

	if useDefaultLogin {
		if asset.Model == "" {
			log.WithFields(logrus.Fields{
				"component":     component,
				"default-login": useDefaultLogin,
				"Asset":         fmt.Sprintf("%+v", asset),
				"Error":         err,
			}).Warn("Unable to use default credentials to connect since asset.Model is unknown.")
			return
		}

		bmcUser = viper.GetString(fmt.Sprintf("bmcDefaults.%s.user", asset.Model))
		bmcPassword = viper.GetString(fmt.Sprintf("bmcDefaults.%s.password", asset.Model))
	} else {
		bmcUser = viper.GetString("bmcUser")
		bmcPassword = viper.GetString("bmcPassword")
	}

	bmcConnection, err = discover.ScanAndConnect(asset.IpAddress, bmcUser, bmcPassword)
	if err != nil {
		log.WithFields(logrus.Fields{
			"component":     component,
			"default-login": useDefaultLogin,
			"Asset":         fmt.Sprintf("%+v", asset),
			"Error":         err,
		}).Warn("Unable to connect to bmc.")
		return bmcConnection, err
	}

	return bmcConnection, err

}

//runs one time setup actions.
// TODO: read in resource config for needs setup from cfg dif
func (b *Butler) setupAsset(id int, config *cfgresources.ResourcesConfig, asset *asset.Asset) {
	fmt.Printf("--> Asset to setup : %+v", asset)
}

// applyConfig setups up the bmc connection,
//
// and iterates over the config to be applied.
func (b *Butler) applyConfig(id int, config *cfgresources.ResourcesConfig, asset *asset.Asset) {

	var useDefaultLogin bool
	var err error
	log := b.Log
	component := "butler-apply-config"

	//this bit is ugly, but I need a way to retry connecting and login,
	//without having to pass around the specific bmc/chassis types (*m1000.M1000e etc..),
	//maybe this can be done in bmclib instead.
	client, err := b.connectAsset(asset, useDefaultLogin)
	if err != nil {
		return
	}

	switch deviceType := client.(type) {
	case devices.Bmc:

		bmc := client.(devices.Bmc)
		asset.Model = bmc.ModelId()

		err = bmc.Login()
		//if the first attempt to login fails, try with default creds
		if err != nil {
			log.WithFields(logrus.Fields{
				"component":         component,
				"butler-id":         id,
				"device-type":       deviceType,
				"use-default-login": useDefaultLogin,
				"Serial":            asset.Serial,
				"IP":                asset.IpAddress,
				"Vendor":            asset.Vendor,
				"Model":             asset.Model,
				"Type":              asset.Type,
				"Error":             err,
			}).Warn("Unable to login with current credentials, attempting default login..")

			useDefaultLogin = true
			client, err = b.connectAsset(asset, useDefaultLogin)
			if err != nil {
				return
			}

			bmc = client.(devices.Bmc)
			err = bmc.Login()

			//all attempts to login have failed.
			if err != nil {
				log.WithFields(logrus.Fields{
					"component":         component,
					"butler-id":         id,
					"use-default-login": useDefaultLogin,
					"Serial":            asset.Serial,
					"IP":                asset.IpAddress,
					"Vendor":            asset.Vendor,
					"Model":             asset.Model,
					"Type":              asset.Type,
					"Error":             err,
				}).Warn("Unable to login with default credentials.")
				return
			}

		} else {
			log.WithFields(logrus.Fields{
				"component":         component,
				"butler-id":         id,
				"use-default-login": useDefaultLogin,
				"Serial":            asset.Serial,
				"IP":                asset.IpAddress,
				"Vendor":            asset.Vendor,
				"Model":             asset.Model,
				"Type":              asset.Type,
			}).Info("Logged into asset.")
		}

		bmc.ApplyCfg(config)
		bmc.Logout()
	case devices.BmcChassis:
		chassis := client.(devices.BmcChassis)
		asset.Model = chassis.ModelId()

		err := chassis.Login()
		//if the first attempt to login fails, try with default creds
		if err != nil {
			log.WithFields(logrus.Fields{
				"component":         component,
				"butler-id":         id,
				"use-default-login": useDefaultLogin,
				"Asset":             fmt.Sprintf("%+v", asset),
				"Error":             err,
			}).Warn("Unable to login to bmc with current credentials, trying default login..")

			useDefaultLogin = true
			client, err = b.connectAsset(asset, useDefaultLogin)
			if err != nil {
				return
			}

			chassis = client.(devices.BmcChassis)
			err = chassis.Login()

			//all attempts to login have failed.
			if err != nil {
				log.WithFields(logrus.Fields{
					"component":         component,
					"butler-id":         id,
					"use-default-login": useDefaultLogin,
					"Asset":             fmt.Sprintf("%+v", asset),
					"Error":             err,
				}).Warn("Unable to login to bmc with default credentials")
				return
			}

		} else {
			log.WithFields(logrus.Fields{
				"component":         component,
				"butler-id":         id,
				"use-default-login": useDefaultLogin,
				"Asset":             fmt.Sprintf("%+v", asset),
			}).Info("Successfully logged into asset.")
		}

		chassis.ApplyCfg(config)
		log.WithFields(logrus.Fields{
			"component": component,
			"butler-id": id,
			"Asset":     fmt.Sprintf("%+v", asset),
		}).Info("Config applied.")

		chassis.Logout()
	default:
		log.WithFields(logrus.Fields{
			"component": component,
			"butler-id": id,
			"Asset":     fmt.Sprintf("%+v", asset),
		}).Warn("Unkown device type.")
		return
	}

	return

}
