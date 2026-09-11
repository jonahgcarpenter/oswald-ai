// Package documents adapts scoped document library management to slash commands.
package documents

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
)

const usage = "/documents usage [owner-cursor] | list [cursor] | forget <id|all> | confirm <code>"
const maxSnapshot = 10000
const confirmationLifetime = 5 * time.Minute

var errSnapshotTooLarge = errors.New("document snapshot exceeds confirmation limit")

type confirmation struct {
	principal identity.Principal
	scope     memory.DocumentScope
	expires   time.Time
	owners    map[string][]string
	count     int
}

type handler struct {
	accounts *accounts.Service
	store    *memory.Store
	disk     storageStats
	now      func() time.Time
	mu       sync.Mutex
	pending  map[[32]byte]confirmation
}

type storageStats interface {
	UserDocumentStorageStats(context.Context) (memory.DocumentStorageStats, error)
}

// New creates a handler with bounded, process-local, one-use confirmations.
func New(accounts *accounts.Service, store *memory.Store) commands.Handler {
	disk, _ := any(store).(storageStats)
	return &handler{accounts: accounts, store: store, disk: disk, now: time.Now, pending: make(map[[32]byte]confirmation)}
}

func (*handler) Definition() commands.Definition {
	return commands.Definition{Name: "documents", Summary: "Manage your documents; administrators automatically manage all libraries.", Usage: usage, UserExclusive: true}
}

func (h *handler) scope(req commands.Request) (memory.DocumentScope, error) {
	if h.accounts == nil || h.store == nil {
		return memory.DocumentScope{}, fmt.Errorf("document service is unavailable")
	}
	owner, err := h.accounts.ResolvePrincipal(req.Principal)
	if err != nil {
		return memory.DocumentScope{}, err
	}
	if owner != req.Principal.CanonicalUserID {
		return memory.DocumentScope{}, accounts.ErrPrincipalMismatch
	}
	admin, err := h.accounts.IsAdminPrincipal(req.Principal)
	if err != nil {
		return memory.DocumentScope{}, err
	}
	if admin {
		return memory.DocumentScope{Global: true, IncludeExpired: true}, nil
	}
	return memory.DocumentScope{UserID: owner, IncludeExpired: true}, nil
}

// ResolveFenceTargets authorizes before resolving targets. Confirmation deletes
// retain the snapshot's owner predicates, even if ownership changes while queued.
func (h *handler) ResolveFenceTargets(ctx context.Context, req commands.Request) ([]string, error) {
	scope, err := h.scope(req)
	if err != nil || !scope.Global || len(req.Args) != 2 {
		return nil, err
	}
	var owners []string
	if req.Args[0] == "confirm" {
		h.mu.Lock()
		c, ok := h.pending[sha256.Sum256([]byte(req.Args[1]))]
		h.mu.Unlock()
		if ok && c.principal == req.Principal && c.scope == scope && h.now().Before(c.expires) {
			for owner := range c.owners {
				owners = append(owners, owner)
			}
		}
	} else if req.Args[0] == "forget" && req.Args[1] != "all" {
		// An exact-ID global delete may follow an ownership move. Fence the
		// current account graph so a merge cannot move it to an unfenced owner.
		users, err := h.accounts.ListUsers()
		if err != nil {
			return nil, err
		}
		for _, user := range users {
			owners = append(owners, user.CanonicalUserID)
		}
	}
	sort.Strings(owners)
	return owners, nil
}

func (h *handler) Execute(ctx context.Context, req commands.Request) (commands.Result, error) {
	scope, err := h.scope(req)
	if err != nil {
		return commands.Result{}, err
	}
	if len(req.Args) == 0 || len(req.Args) > 2 {
		return rejected(commands.UsageText(h.Definition()), "invalid_arguments"), nil
	}
	after := ""
	if len(req.Args) == 2 {
		after = req.Args[1]
	}
	switch req.Args[0] {
	case "usage":
		if after != "" && !scope.Global {
			return rejected("Owner pagination is only available in administrator scope.", "invalid_arguments"), nil
		}
		return h.usage(ctx, scope, after)
	case "list":
		page, err := h.store.PageUserDocuments(ctx, scope, after, 10)
		if err != nil {
			return commands.Result{}, err
		}
		var text strings.Builder
		fmt.Fprintf(&text, "%s documents (includes expired retained documents):\n", scopeLabel(scope))
		for _, d := range page.Documents {
			fmt.Fprintf(&text, "ID: %s\nFile: %q\nStatus: %s; source: %d bytes; text: %d bytes; expires: %s\n", d.ID, d.Filename, d.Status, d.SourceBytes, d.TextBytes, d.ExpiresAt.UTC().Format(time.RFC3339))
			if !h.now().Before(d.ExpiresAt) {
				text.WriteString("Expired: retained pending deletion; unavailable to document tools.\n")
			}
			if scope.Global {
				fmt.Fprintf(&text, "Canonical owner: %s\n", d.UserID)
			}
		}
		if len(page.Documents) == 0 {
			text.WriteString("No documents on this page.\n")
		}
		if page.HasMore {
			fmt.Fprintf(&text, "Next: /documents list %s", page.NextAfter)
		}
		return commands.Result{Text: text.String()}, nil
	case "forget":
		if after == "" {
			break
		}
		if after != "all" {
			deleted, err := h.store.DeleteUserDocumentsFenced(ctx, scope, []string{after}, req.FencedUserIDs)
			if errors.Is(err, memory.ErrDocumentUnfenced) {
				return rejected("Document ownership or authorization changed after scheduling. Nothing was deleted. Send the command again.", "fence_required"), nil
			}
			if errors.Is(err, memory.ErrDocumentNotFound) {
				return rejected("Document not found in your current scope.", "not_found"), nil
			}
			if err != nil {
				return commands.Result{}, err
			}
			return deletionResult(deleted.DeletedCount, deleted.DeletedCount, false), nil
		}
		docs, err := h.snapshot(ctx, scope)
		if errors.Is(err, errSnapshotTooLarge) {
			return rejected("This scope exceeds the 10,000-document confirmation limit. No confirmation was created and nothing was deleted. Forget individual documents before trying again.", "capacity_exceeded"), nil
		}
		if err != nil {
			return commands.Result{}, err
		}
		if len(docs) == 0 {
			return commands.Result{Text: "No retained documents to forget. In-flight upload reservations are not canceled."}, nil
		}
		c := confirmation{principal: req.Principal, scope: scope, expires: h.now().Add(confirmationLifetime), owners: make(map[string][]string), count: len(docs)}
		for _, d := range docs {
			c.owners[d.UserID] = append(c.owners[d.UserID], d.ID)
		}
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return commands.Result{}, err
		}
		code := hex.EncodeToString(token[:])
		h.mu.Lock()
		defer h.mu.Unlock()
		for key, old := range h.pending {
			if !h.now().Before(old.expires) || old.principal == req.Principal {
				delete(h.pending, key)
			}
		}
		retained := 0
		for _, old := range h.pending {
			retained += old.count
		}
		if retained+c.count > maxSnapshot || len(h.pending) >= 100 {
			return rejected("Too many pending document confirmations. Try again in five minutes.", "capacity_exceeded"), nil
		}
		h.pending[sha256.Sum256([]byte(code))] = c
		return commands.Result{Text: fmt.Sprintf("%s scope: forget exactly %d retained documents captured during pagination, including expired documents. Later uploads are not included and in-flight upload reservations are not canceled. Deletion uses owner-scoped batches of at most 50 and can partially complete if a batch fails. Confirm within five minutes: /documents confirm %s", scopeLabel(scope), c.count, code)}, nil
	case "confirm":
		if after == "" {
			break
		}
		key := sha256.Sum256([]byte(after))
		h.mu.Lock()
		c, ok := h.pending[key]
		if ok && c.principal == req.Principal {
			delete(h.pending, key)
		} else {
			ok = false
		}
		h.mu.Unlock()
		if !ok || c.scope != scope || !h.now().Before(c.expires) {
			return rejected("Confirmation is invalid, expired, already used, or your authorization changed. Start /documents forget all again.", "invalid_confirmation"), nil
		}
		heldOwners := make(map[string]bool, len(req.FencedUserIDs))
		for _, owner := range req.FencedUserIDs {
			heldOwners[owner] = true
		}
		owners := make([]string, 0, len(c.owners))
		for owner := range c.owners {
			if !heldOwners[owner] {
				return rejected("The selected owners are not all fenced for this execution. Nothing was deleted. Start /documents forget all again.", "fence_required"), nil
			}
			owners = append(owners, owner)
		}
		sort.Strings(owners)
		deleted := 0
		for _, owner := range owners {
			ids := c.owners[owner]
			for len(ids) > 0 {
				// Refresh authorization for every committed batch. Never broaden
				// an old private confirmation following an administrator grant.
				current, err := h.scope(req)
				if err != nil || current != c.scope {
					return deletionResult(deleted, c.count, true), nil
				}
				n := min(50, len(ids))
				out, err := h.store.DeleteUserDocumentsFenced(ctx, memory.DocumentScope{UserID: owner, IncludeExpired: true}, ids[:n], []string{owner})
				if err != nil {
					return deletionResult(deleted, c.count, true), nil
				}
				deleted += out.DeletedCount
				ids = ids[n:]
			}
		}
		return deletionResult(deleted, c.count, false), nil
	}
	return rejected(commands.UsageText(h.Definition()), "invalid_arguments"), nil
}

func (h *handler) snapshot(ctx context.Context, scope memory.DocumentScope) ([]memory.UserDocument, error) {
	var docs []memory.UserDocument
	after := ""
	for {
		page, err := h.store.PageUserDocuments(ctx, scope, after, 50)
		if err != nil {
			return nil, err
		}
		if len(docs)+len(page.Documents) > maxSnapshot {
			return nil, errSnapshotTooLarge
		}
		docs = append(docs, page.Documents...)
		if !page.HasMore {
			return docs, nil
		}
		after = page.NextAfter
	}
}

func (h *handler) usage(ctx context.Context, scope memory.DocumentScope, after string) (commands.Result, error) {
	u, err := h.store.UserDocumentUsage(ctx, scope)
	if err != nil {
		return commands.Result{}, err
	}
	sourceLimit, textLimit := int64(memory.DocumentMaxUserSourceBytes), int64(memory.DocumentMaxUserTextBytes)
	countLimit := fmt.Sprint(memory.DocumentMaxUserCount)
	if scope.Global {
		sourceLimit, textLimit = memory.DocumentMaxGlobalSourceBytes, memory.DocumentMaxGlobalTextBytes
		countLimit = "no global count cap"
	}
	var text strings.Builder
	fmt.Fprintf(&text, "%s document usage (bytes; includes expired retained rows):\nDocuments: %d / %s\nReserved document slots: %d\nDocuments + reserved slots: %d / %s\nSource: %d / %d\nReserved source: %d\nSource + reserved: %d / %d\nExtracted text: %d\nReserved text: %d (extraction: %d; upload: %d)\nText + reserved: %d / %d\nLive upload reservations: %d\nExpired retained documents: %d\nLive statuses: queued=%d running=%d failed=%d ready=%d partial=%d\n", scopeLabel(scope), u.DocumentCount, countLimit, u.ReservedDocumentCount, u.DocumentCount+u.ReservedDocumentCount, countLimit, u.SourceBytes, sourceLimit, u.ReservedSourceBytes, u.SourceBytes+u.ReservedSourceBytes, sourceLimit, u.TextBytes, u.ReservedTextBytes, u.ReservedTextBytes-u.UploadReservedTextBytes, u.UploadReservedTextBytes, u.TextBytes+u.ReservedTextBytes, textLimit, u.ReservationCount, u.ExpiredCount, u.QueuedCount, u.RunningCount, u.FailedCount, u.ReadyCount, u.PartialCount)
	text.WriteString("Status counts exclude expired documents. Upload reservations are already included in the combined quota totals.\n")
	if scope.Global {
		if h.disk == nil {
			text.WriteString("Server storage statistics: unavailable.\n")
		} else {
			stats, err := h.disk.UserDocumentStorageStats(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return commands.Result{}, ctx.Err()
				}
				text.WriteString("Server storage statistics: unavailable.\n")
			} else {
				fmt.Fprintf(&text, "Server database: %d bytes; WAL: %d bytes (whole application, not document-only).\n", stats.DatabaseBytes, stats.WALBytes)
				if stats.DiskKnown {
					fmt.Fprintf(&text, "Filesystem available space: %d bytes.\n", stats.FreeBytes)
				} else {
					text.WriteString("Filesystem available space: unknown.\n")
				}
			}
		}
		users, err := h.accounts.ListUsers()
		if err != nil {
			return commands.Result{}, err
		}
		shown, last := 0, ""
		text.WriteString("Per-user usage (canonical owner; independently sampled):\n")
		for _, user := range users {
			if user.CanonicalUserID <= after {
				continue
			}
			if shown == 10 {
				fmt.Fprintf(&text, "Next: /documents usage %s\n", last)
				break
			}
			ownerUsage, err := h.store.UserDocumentUsage(ctx, memory.DocumentScope{UserID: user.CanonicalUserID, IncludeExpired: true})
			if err != nil {
				return commands.Result{}, err
			}
			fmt.Fprintf(&text, "%s: documents=%d+%d reserved/%d source=%d+%d reserved/%d text=%d reserved=%d (extraction=%d upload=%d) combined=%d/%d upload_reservations=%d expired=%d live: queued=%d running=%d failed=%d ready=%d partial=%d\n", user.CanonicalUserID, ownerUsage.DocumentCount, ownerUsage.ReservedDocumentCount, memory.DocumentMaxUserCount, ownerUsage.SourceBytes, ownerUsage.ReservedSourceBytes, memory.DocumentMaxUserSourceBytes, ownerUsage.TextBytes, ownerUsage.ReservedTextBytes, ownerUsage.ReservedTextBytes-ownerUsage.UploadReservedTextBytes, ownerUsage.UploadReservedTextBytes, ownerUsage.TextBytes+ownerUsage.ReservedTextBytes, memory.DocumentMaxUserTextBytes, ownerUsage.ReservationCount, ownerUsage.ExpiredCount, ownerUsage.QueuedCount, ownerUsage.RunningCount, ownerUsage.FailedCount, ownerUsage.ReadyCount, ownerUsage.PartialCount)
			shown++
			last = user.CanonicalUserID
		}
	}
	return commands.Result{Text: text.String()}, nil
}

func scopeLabel(scope memory.DocumentScope) string {
	if scope.Global {
		return "Administrator global"
	}
	return "Your"
}

func rejected(text, reason string) commands.Result {
	return commands.Result{Text: text, Outcome: commands.Outcome{Status: "rejected", ReasonCode: reason}}
}

func deletionResult(deleted, total int, failed bool) commands.Result {
	result := commands.Result{Text: fmt.Sprintf("Deleted %d of %d selected documents. Sources, extracted text, and extraction jobs were removed. Transcripts, delivered messages, external logs, and backups are not erased.", deleted, total), Outcome: commands.Outcome{Status: "ok", Operation: "document.forget", IsChanged: deleted > 0, AffectedCount: deleted}}
	if failed {
		result.Text += " Deletion stopped before completion. Earlier committed batches were NOT rolled back; remaining selected documents were not deleted by this command. The confirmation is consumed. Check /documents list and start a new request."
		result.Outcome.Status = "error"
		result.Outcome.ReasonCode = "operation_failed"
	}
	return result
}
