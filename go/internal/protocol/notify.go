package protocol

// Notifications with ids, actions and closes (the guest's
// org.freedesktop.Notifications daemon, `pocketnix-guest notifyd`). A guest
// [GuestNotify] is answered with the notification's id ([GuestReply].ID),
// which the agent assigns, and its [TypeNotify] event carries it as
// Event.Notification; Android's actions and dismissals come back as control
// requests, which the agent hands to every [GuestSubscribe] connection.

const (
	// GuestNotifyClose withdraws notification ID (CloseNotification, or an
	// expiry); the app gets a [TypeNotifyClose] event.
	GuestNotifyClose = "notify-close"
	// GuestSubscribe is answered once, then the connection carries
	// [GuestEvent] lines until either side closes it. No idle deadline applies.
	GuestSubscribe = "subscribe"
)

// TypeNotifyClose (agent → app event) withdraws Event.Notification.
const TypeNotifyClose = "notify-close"

// App → agent requests about a notification the app posted, in
// Request.Notification.
const (
	// TypeNotifyAction reports that the user chose Request.Key; the app has
	// removed the notification.
	TypeNotifyAction = "notify-action"
	// TypeNotifyClosed reports that the notification went away with
	// Request.Reason (a Closed* value).
	TypeNotifyClosed = "notify-closed"
)

// Why a notification closed, as org.freedesktop.Notifications numbers them.
const (
	ClosedExpired   = 1
	ClosedDismissed = 2
	ClosedByCall    = 3
)

// Urgencies, in GuestRequest.Urgency and Event.Urgency; empty is normal.
const (
	UrgencyLow      = "low"
	UrgencyNormal   = "normal"
	UrgencyCritical = "critical"
)

// NotifyAction is one of a notification's actions. Key "default" is the
// notification's own tap rather than a button.
type NotifyAction struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// GuestEvent is pushed on a [GuestSubscribe] connection: Type is
// TypeNotifyAction (with Key) or TypeNotifyClosed (with Reason).
type GuestEvent struct {
	Type   string `json:"type"`
	ID     uint32 `json:"id"`
	Key    string `json:"key,omitempty"`
	Reason uint32 `json:"reason,omitempty"`
}
