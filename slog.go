package monitor

import (
	"context"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
)

// SlogOptions configures NewSlogHandler.
type SlogOptions struct {
	// Level is the minimum level forwarded to Monitor. Records below it still
	// reach the wrapped handler. Default: slog.LevelInfo.
	Level slog.Leveler

	// EventKey names the attribute that carries the Monitor event name; it is
	// removed from the event's data. Default: "event". A record without one is
	// emitted as "log.<level>" (log.info, log.warn, log.error), with the
	// message in data.message.
	EventKey string
}

// NewSlogHandler returns a slog.Handler that passes every record to next —
// typically a TextHandler or JSONHandler on stdout — and also emits records at
// or above Level to Monitor.
//
// It lets a service that already logs through slog report to Monitor without
// touching its call sites:
//
//	base := slog.NewJSONHandler(os.Stdout, nil)
//	slog.SetDefault(slog.New(monitor.NewSlogHandler(base, nil)))
//	slog.ErrorContext(ctx, "snapshot upload failed",
//		"event", "snapshot.upload.failed", "error", err)
//
// Use the Context variants (slog.InfoContext, …) so the event carries the
// request_id, trace_id and user_id of the request it belongs to. next may be
// nil to send records to Monitor only.
//
// Slog messages are free text, so for anything worth grouping pass an "event"
// attribute named {resource}.{action}.{result}. Without one, every error a
// service logs shares the name log.error and only its message tells it apart.
func NewSlogHandler(next slog.Handler, opts *SlogOptions) slog.Handler {
	h := &slogHandler{next: next, level: slog.LevelInfo, eventKey: "event"}
	if opts != nil {
		if opts.Level != nil {
			h.level = opts.Level
		}
		if opts.EventKey != "" {
			h.eventKey = opts.EventKey
		}
	}
	return h
}

type slogHandler struct {
	next     slog.Handler
	level    slog.Leveler
	eventKey string
	pre      map[string]any // attributes from WithAttrs, keys already group-qualified
	prefix   string         // "group.subgroup." from WithGroup
}

func (h *slogHandler) clone() *slogHandler {
	c := *h
	c.pre = make(map[string]any, len(h.pre))
	for k, v := range h.pre {
		c.pre[k] = v
	}
	return &c
}

func (h *slogHandler) Enabled(ctx context.Context, l slog.Level) bool {
	if l >= h.level.Level() {
		return true
	}
	return h.next != nil && h.next.Enabled(ctx, l)
}

func (h *slogHandler) Handle(ctx context.Context, r slog.Record) error {
	var err error
	if h.next != nil && h.next.Enabled(ctx, r.Level) {
		err = h.next.Handle(ctx, r)
	}
	if r.Level < h.level.Level() {
		return err
	}

	data := make(map[string]any, len(h.pre)+r.NumAttrs()+4)
	for k, v := range h.pre {
		data[k] = v
	}
	r.Attrs(func(a slog.Attr) bool {
		addSlogAttr(data, h.prefix, a)
		return true
	})

	level := slogToMonitorLevel(r.Level)
	name := "log." + level
	for _, key := range []string{h.eventKey, h.prefix + h.eventKey} {
		if s, ok := data[key].(string); ok && s != "" {
			name = s
			delete(data, key)
			break
		}
	}
	data["message"] = r.Message

	if r.PC != 0 {
		if cfg := globalConfig.Load(); cfg == nil || captureSourceEnabled(cfg) {
			frame, _ := runtime.CallersFrames([]uintptr{r.PC}).Next()
			fn := frame.Function
			if i := strings.LastIndex(fn, "."); i >= 0 {
				fn = fn[i+1:]
			}
			data["source_file"] = filepath.Base(frame.File)
			data["source_line"] = frame.Line
			data["source_func"] = fn
		}
	}

	if ctx == nil {
		ctx = context.Background()
	}
	emitInternal(ctx, name, data, level)
	return err
}

func (h *slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	c := h.clone()
	for _, a := range attrs {
		addSlogAttr(c.pre, h.prefix, a)
	}
	if h.next != nil {
		c.next = h.next.WithAttrs(attrs)
	}
	return c
}

func (h *slogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := h.clone()
	c.prefix = h.prefix + name + "."
	if h.next != nil {
		c.next = h.next.WithGroup(name)
	}
	return c
}

// addSlogAttr flattens a into data under prefix. Groups become dotted keys
// ("db.instance"), which monitor-core can filter and group on.
func addSlogAttr(data map[string]any, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p = prefix + a.Key + "."
		}
		for _, ga := range a.Value.Group() {
			addSlogAttr(data, p, ga)
		}
		return
	}
	data[prefix+a.Key] = a.Value.Any()
}

// slogToMonitorLevel maps slog's integer levels onto Monitor's five. Anything
// above slog.LevelError+3 — slog has no fatal — is fatal.
func slogToMonitorLevel(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return LevelDebug
	case l < slog.LevelWarn:
		return LevelInfo
	case l < slog.LevelError:
		return LevelWarn
	case l < slog.LevelError+4:
		return LevelError
	default:
		return LevelFatal
	}
}
