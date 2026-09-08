/*
SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
SPDX-License-Identifier: MIT
*/

package web_socket

import (
	"context"
	"math/rand"
	"stackChan/internal/model"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var (
	randMu sync.Mutex
	rander = rand.New(rand.NewSource(time.Now().UnixNano()))
)

const (
	// Allow several missed heartbeat cycles before declaring a device offline.
	// The cleanup job runs every 15 seconds, so this gives the connection up to
	// four checks to recover from Wi-Fi/Cloudflare jitter.
	ClientExpireTimeout = 60 * time.Second
)

// StartPingTime sends Ping messages to all connected clients for heartbeat detection
func StartPingTime(ctx context.Context) {
	// Global panic recovery, prevent entire heartbeat detection logic from crashing
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf(ctx, "StartPingTime panic recovered: %v", r)
		}
	}()

	message := createMessage(ping, nil)
	messageType := websocket.BinaryMessage

	// Iterate over StackChanClientPool
	stackChanClientPool.Range(func(_, value any) bool {
		if value == nil {
			return true
		}
		client, ok := value.(*model.StackChanClient)
		if !ok {
			logger.Warningf(ctx, "StartPingTime: invalid type in StackChanClientPool, skip")
			return true
		}
		if client == nil {
			return true
		}

		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf(ctx, "panic in StartPingTime StackChanClientPool forwardMessage: %v", r)
				}
			}()
			if client.GetConn() == nil {
				logger.Debugf(ctx, "StartPingTime: StackChanClient %s has nil conn, skip ping", client.GetMac())
				return
			}
			stackChanSendMessage(ctx, client, &messageType, message)
		}()
		return true // continue iteration
	})

	// Iterate over AppClientPool
	appClientPool.Range(func(_, value any) bool {
		if value == nil {
			return true
		}
		clients, ok := value.([]*model.AppClient)
		if !ok {
			logger.Warningf(ctx, "StartPingTime: invalid type in AppClientPool, skip")
			return true
		}
		if len(clients) == 0 {
			return true
		}

		for _, client := range clients {
			func() {
				defer func() {
					if r := recover(); r != nil {
						logger.Errorf(ctx, "panic in StartPingTime AppClientPool forwardMessage: %v", r)
					}
				}()
				if client == nil {
					return
				}
				if client.GetConn() == nil {
					logger.Debugf(ctx, "StartPingTime: AppClient %s (deviceId: %s) has nil conn, skip ping", client.GetMac(), client.GetDeviceId())
					return
				}
				appSendMessage(ctx, client, &messageType, message)
			}()
		}
		return true // continue iteration
	})
}

// CheckExpiredLinks checks and cleans up App client connections that have been inactive for over 60 seconds
func CheckExpiredLinks(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf(ctx, "CheckExpiredLinks panic recovered: %v", r)
		}
	}()

	now := time.Now()
	var expiredClients []*model.AppClient

	// 1. Clean up expired AppClient
	appClientPool.Range(func(mac, value any) bool {
		if mac == nil || value == nil {
			return true
		}
		clients, ok := value.([]*model.AppClient)
		if !ok {
			logger.Warningf(ctx, "AppClientPool invalid type for mac: %v, delete invalid entry", mac)
			appClientPool.Delete(mac)
			return true
		}

		newClients := clients[:0]
		for _, client := range clients {
			if client == nil {
				continue
			}
			if now.Sub(client.GetLastTime()) > ClientExpireTimeout {
				stackChanClientPool.Range(func(_, scValue any) bool {
					defer func() {
						if r := recover(); r != nil {
							logger.Errorf(ctx, "Clean StackChanClient panic: %v", r)
						}
					}()
					stackChanClient, ok := scValue.(*model.StackChanClient)
					if !ok || stackChanClient == nil {
						return true
					}
					if stackChanClient.GetCallAppClient() == client {
						stackChanClient.SetCallAppClient(nil)
					}

					//Remove camera subscription
					removedCamera, cameraEmpty := stackChanClient.RemoveCameraSubscriber(client)
					if removedCamera && cameraEmpty && stackChanClient.GetConn() != nil {
						msg := createMessage(OffCamera, nil)
						msgType := websocket.BinaryMessage
						stackChanSendMessage(ctx, stackChanClient, &msgType, msg)
					}

					//Remove audio subscription
					removedAudio, audioEmpty := stackChanClient.RemoveAudioSubscriber(client)
					if removedAudio && audioEmpty && stackChanClient.GetConn() != nil {
						msg := createMessage(OffAudio, nil)
						msgType := websocket.BinaryMessage
						stackChanSendMessage(ctx, stackChanClient, &msgType, msg)
					}
					return true
				})
				expiredClients = append(expiredClients, client)
			} else {
				newClients = append(newClients, client)
			}
		}
		if len(newClients) == 0 {
			appClientPool.Delete(mac)
		} else {
			appClientPool.Store(mac, newClients)
		}
		return true
	})

	for _, client := range expiredClients {
		if client == nil {
			continue
		}
		logger.Infof(ctx, "Kicked out expired App client: %s", client.GetMac())
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf(ctx, "Close AppClient conn panic: %v", r)
				}
			}()
			conn, generation := client.ConnectionSnapshot()
			if client.ClearConnection(generation) {
				client.CloseWriterCoroutine()
				if conn != nil {
					_ = conn.Close()
				}
			}
		}()
	}

	var expiredStackChanKeys []string
	stackChanClientPool.Range(func(mac, value any) bool {
		if mac == nil || value == nil {
			return true
		}
		macStr, ok := mac.(string)
		if !ok {
			return true
		}
		stackChanClient, ok := value.(*model.StackChanClient)
		if !ok || stackChanClient == nil {
			logger.Warningf(ctx, "StackChanClientPool invalid type for mac: %v, delete invalid entry", macStr)
			stackChanClientPool.Delete(mac)
			return true
		}
		if now.Sub(stackChanClient.GetLastTime()) > ClientExpireTimeout {
			expiredStackChanKeys = append(expiredStackChanKeys, macStr)
		}
		return true
	})

	for _, mac := range expiredStackChanKeys {
		val, ok := stackChanClientPool.Load(mac)
		if !ok {
			continue
		}
		stackChanClient, ok := val.(*model.StackChanClient)
		if !ok || stackChanClient == nil {
			stackChanClientPool.Delete(mac)
			continue
		}

		stackChanClientPool.Delete(mac)

		offlineMsg := createStringMessage(DeviceOffline, "Your StackChan is offline.")
		msgType := websocket.BinaryMessage
		appClients := getAppClients(stackChanClient.GetMac())
		if appClients != nil {
			for _, appClient := range appClients {
				if appClient == nil {
					continue
				}
				func() {
					defer func() {
						if r := recover(); r != nil {
							logger.Errorf(ctx, "Notify AppClient offline panic: %v", r)
						}
					}()
					appSendMessage(ctx, appClient, &msgType, offlineMsg)
				}()
			}
		}

		logger.Infof(ctx, "Kicked out expired StackChan client: %s", mac)

		conn, generation := stackChanClient.ConnectionSnapshot()
		cleared := stackChanClient.ClearConnection(generation)
		if cleared {
			meetingManager.OnDeviceDisconnect(ctx, mac)
			stackChanClient.CloseWriterCoroutine()
		}

		if conn != nil && cleared {
			func() {
				defer func() {
					if r := recover(); r != nil {
						logger.Errorf(ctx, "Close StackChan conn panic: %v", r)
					}
				}()
				_ = conn.Close()
			}()
		}
	}
}

// GetRandomStackChanDevice get Random StackChan Device list
func GetRandomStackChanDevice(userMac string, maxLength int) (list []string) {
	if maxLength <= 0 {
		return []string{}
	}
	var macs []string

	stackChanClientPool.Range(func(key, value interface{}) bool {
		mac := key.(string)
		client := value.(*model.StackChanClient)

		if mac == userMac {
			return true
		}
		online := client.GetConn() != nil
		if online {
			macs = append(macs, mac)
		}

		return true
	})

	if len(macs) == 0 {
		return []string{}
	}

	randMu.Lock()
	rander.Shuffle(len(macs), func(i, j int) {
		macs[i], macs[j] = macs[j], macs[i]
	})
	randMu.Unlock()
	if len(macs) > maxLength {
		macs = macs[:maxLength]
	}

	return macs
}
