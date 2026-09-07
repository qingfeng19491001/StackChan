/*
SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
SPDX-License-Identifier: MIT
*/

package model

import (
	"context"
	"testing"

	"github.com/gorilla/websocket"
)

func TestAppConnectionGenerationPreventsOldHandlerClearingReplacement(t *testing.T) {
	client := &AppClient{}
	first := &websocket.Conn{}
	second := &websocket.Conn{}

	firstGeneration := client.ReplaceConnection(first)
	secondGeneration := client.ReplaceConnection(second)

	if client.ClearConnection(firstGeneration) {
		t.Fatal("old generation cleared the replacement connection")
	}
	if client.GetConn() != second {
		t.Fatal("replacement connection was not retained")
	}
	if !client.ClearConnection(secondGeneration) || client.GetConn() != nil {
		t.Fatal("current generation was not cleared")
	}
}

func TestStackChanConnectionGenerationPreventsOldHandlerClearingReplacement(t *testing.T) {
	client := &StackChanClient{}
	firstGeneration := client.ReplaceConnection(&websocket.Conn{})
	second := &websocket.Conn{}
	secondGeneration := client.ReplaceConnection(second)

	if client.ClearConnection(firstGeneration) {
		t.Fatal("old generation cleared the replacement connection")
	}
	if client.GetConn() != second {
		t.Fatal("replacement connection was not retained")
	}
	if !client.ClearConnection(secondGeneration) {
		t.Fatal("current generation was not cleared")
	}
}

func TestAudioSubscribersAreAtomicAndIdempotent(t *testing.T) {
	stackChan := &StackChanClient{}
	app := &AppClient{}

	if !stackChan.AddAudioSubscriber(app) {
		t.Fatal("first subscription was not added")
	}
	if stackChan.AddAudioSubscriber(app) {
		t.Fatal("duplicate subscription was added")
	}
	if got := len(stackChan.GetAudioSubscriptionList()); got != 1 {
		t.Fatalf("got %d subscriptions, want 1", got)
	}
	removed, empty := stackChan.RemoveAudioSubscriber(app)
	if !removed || !empty {
		t.Fatalf("remove result = (%v, %v), want (true, true)", removed, empty)
	}
	removed, empty = stackChan.RemoveAudioSubscriber(app)
	if removed || !empty {
		t.Fatalf("second remove result = (%v, %v), want (false, true)", removed, empty)
	}
}

func TestTrySendReportsBackpressureAndClosure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &AppClient{conn: &websocket.Conn{}, connGeneration: 1, sendChan: make(chan *WsSendMsg, 1), ctx: ctx}
	message := &WsSendMsg{MsgType: websocket.BinaryMessage, Data: []byte{1}}

	if got := client.TrySend(message); got != SendEnqueued {
		t.Fatalf("first send result = %v, want SendEnqueued", got)
	}
	if got := client.TrySend(message); got != SendQueueFull {
		t.Fatalf("second send result = %v, want SendQueueFull", got)
	}
	cancel()
	if got := client.TrySend(message); got != SendClosed {
		t.Fatalf("send after cancellation = %v, want SendClosed", got)
	}
	if got := client.TrySend(nil); got != SendInvalid {
		t.Fatalf("nil send result = %v, want SendInvalid", got)
	}
}

func TestStaleGenerationCannotEnterReplacementSendQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &AppClient{conn: &websocket.Conn{}, connGeneration: 1, sendChan: make(chan *WsSendMsg, 1), ctx: ctx}
	oldGeneration := client.ConnectionGeneration()
	client.ReplaceConnection(&websocket.Conn{})

	if got := client.TrySendForGeneration(oldGeneration, &WsSendMsg{MsgType: websocket.BinaryMessage, Data: []byte{1}}); got != SendUnavailable {
		t.Fatalf("stale generation send = %v, want %v", got, SendUnavailable)
	}
	if got := len(client.sendChan); got != 0 {
		t.Fatalf("stale message entered replacement queue: %d", got)
	}
}
