package app

import (
	"context"
	"errors"

	"github.com/gosuda/flats/internal/buildinfo"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
	"github.com/gosuda/flats/internal/webhook"
)

// startWebhook builds the sender of the event webhook, gives it to the service and runs it with the host's other
// background work (docs/spec/event-webhook.md, rules 6, 11 and 17). The host calls it after core.New returns and
// before the management server accepts a request, so the events that core.New makes have no sender and are not sent.
// Close stops the sender before it closes the store.
func (h *Host) startWebhook(ctx context.Context, keyFile core.KeyFile, st *store.Store, logf func(string, ...any)) {
	sender := webhook.New(webhook.Config{
		Keys:    func(ctx context.Context) (webhook.Credentials, error) { return webhookCredentials(ctx, keyFile, st) },
		Logf:    logf,
		Version: buildinfo.Get().Version,
	})
	h.Svc.SetNotifier(sender)
	h.background(sender.Run)
	logLonelyWebhookKey(ctx, st, logf)
}

// webhookCredentials reads and opens the two webhook keys. A missing key is webhook.ErrOff, which drops the event
// without a log line. A value that does not open is an error, which logs one line (rule 11). Both rows are read before
// either is opened, so a host with the webhook off, also one that holds only one of the keys, reads no key file and
// decrypts nothing for an event.
func webhookCredentials(ctx context.Context, keyFile core.KeyFile, st *store.Store) (webhook.Credentials, error) {
	var rows [2]store.HostKey
	for i, id := range []string{core.HostKeyWebhookURL, core.HostKeyWebhookSecret} {
		k, err := st.GetHostKey(ctx, id)
		if errors.Is(err, store.ErrNotFound) || err == nil && !k.HasValue() {
			return webhook.Credentials{}, webhook.ErrOff
		}
		if err != nil {
			return webhook.Credentials{}, err
		}
		rows[i] = k
	}
	url, err := core.OpenHostKey(keyFile, rows[0])
	if err != nil {
		return webhook.Credentials{}, err
	}
	secret, err := core.OpenHostKey(keyFile, rows[1])
	if err != nil {
		return webhook.Credentials{}, err
	}
	return webhook.Credentials{URL: url, Secret: secret}, nil
}

// logLonelyWebhookKey logs one line at the start when only one of the two webhook keys has a value, and says which one
// is missing (rule 6). A host with neither key, the default, logs nothing.
func logLonelyWebhookKey(ctx context.Context, st *store.Store, logf func(string, ...any)) {
	has := func(id string) bool {
		k, err := st.GetHostKey(ctx, id)
		return err == nil && k.HasValue()
	}
	url, secret := has(core.HostKeyWebhookURL), has(core.HostKeyWebhookSecret)
	switch {
	case url && !secret:
		logf("webhook: the %s key is not stored, so no event is sent; store it with `flats key set %s` while the host is stopped", core.HostKeyWebhookSecret, core.HostKeyWebhookSecret)
	case secret && !url:
		logf("webhook: the %s key is not stored, so no event is sent; store it with `flats key set %s` while the host is stopped", core.HostKeyWebhookURL, core.HostKeyWebhookURL)
	}
}
