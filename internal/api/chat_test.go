package api

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/103mail/webmail-server/internal/store"
)

// Reading a chat is PUT on /chat/read/{chatId}, so a client without a socket,
// and the end-to-end stories, can do what the socket's read frame does: clear
// the reader's unread count and count as read in the sender's ticks.
func TestReadingAChatThroughTheAPI(t *testing.T) {
	a := newAPIStack(t)
	alice, _ := a.account("alice@example.test", "Alice")
	bob, _ := a.account("bob@example.test", "Bob")
	ctx := context.Background()
	mailboxOf := func(customerID int64) int64 {
		id, err := store.SelectInt(ctx, a.db, `SELECT "id" FROM "mailbox" WHERE "customer_id" = ? LIMIT 1`, customerID)
		if err != nil || id == 0 {
			t.Fatalf("mailbox of %d = %d, %v", customerID, id, err)
		}
		return id
	}
	aliceChat, err := a.chat.AddChat(ctx, alice, mailboxOf(alice), "direct", nil, []string{"bob@example.test"})
	if err != nil || aliceChat == 0 {
		t.Fatalf("AddChat = %d, %v", aliceChat, err)
	}
	if _, err := a.chat.AddMessage(ctx, alice, aliceChat, "Lunch?", "", nil); err != nil {
		t.Fatal(err)
	}
	chats, err := a.chat.ListChats(ctx, bob, mailboxOf(bob), "")
	if err != nil || len(chats) != 1 || chats[0].UnreadCount != 1 {
		t.Fatalf("Bob's chats = %+v, %v", chats, err)
	}
	bobChat := strconv.FormatInt(chats[0].ChatID, 10)

	if status, _ := a.call("bob@example.test", http.MethodPut, "/api/v1/chat/read/"+bobChat, `{}`); status != 200 {
		t.Fatalf("Bob reading answered %d", status)
	}
	if chats, _ := a.chat.ListChats(ctx, bob, mailboxOf(bob), ""); chats[0].UnreadCount != 0 {
		t.Errorf("Bob has %d unread after reading", chats[0].UnreadCount)
	}
	messages, err := a.chat.ListMessages(ctx, alice, aliceChat, "")
	if err != nil || len(messages) != 1 || messages[0].DeliveryStatus == nil || messages[0].DeliveryStatus.Read != 2 {
		t.Errorf("Alice's message after Bob read it = %+v, %v", messages, err)
	}

	if status, _ := a.call("bob@example.test", http.MethodGet, "/api/v1/chat/read/"+bobChat, ""); status != 405 {
		t.Errorf("GET on read answered %d, want 405", status)
	}
}
