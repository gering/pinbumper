// Package run plans and applies image-pin bumps.
package run

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gering/pinbumper/internal/compose"
	"github.com/gering/pinbumper/internal/portainer"
	"github.com/gering/pinbumper/internal/registry"
)

// Mode is plan (dry-run) or apply (mutate).
type Mode int

const (
	Plan Mode = iota
	Apply
)

func (m Mode) String() string {
	if m == Apply {
		return "apply"
	}
	return "plan"
}

// Source identifies where a compose document came from.
type Source struct {
	Kind      string // "local" or "portainer"
	Name      string
	Path      string
	Stack     portainer.Stack
	HasStack  bool
	HasHealth map[string]bool
}

// Decision is one labeled service's plan.
type Decision struct {
	Source  Source
	Service compose.Service
	From    string
	To      string
	NewRef  string
	Changed bool
	Reason  string
	Err     error
}

// Options configure a run.
type Options struct {
	Mode          Mode
	ComposeFiles  []string
	Portainer     *portainer.Client
	StackFilter   []string
	Tags          registry.Lister
	Deploy        Deployer
	SkipDeploy    bool
	PullImage     bool
	HealthTimeout time.Duration
	PollEvery     time.Duration
	Stdout        io.Writer
	Stderr        io.Writer
}

// Deployer recreates local compose services after a file rewrite.
type Deployer interface {
	Up(ctx context.Context, composeFile string, services []string) error
	Health(ctx context.Context, composeFile, service string) (Health, error)
}

// Health is a container's runtime health.
type Health struct {
	Found    bool
	Running  bool
	HasCheck bool
	Status   string
	ExitCode int
	State    string
}

func (o Options) out() io.Writer {
	if o.Stdout != nil {
		return o.Stdout
	}
	return os.Stdout
}

func (o Options) errw() io.Writer {
	if o.Stderr != nil {
		return o.Stderr
	}
	return os.Stderr
}

// Run discovers targets, plans bumps, and optionally applies them.
func Run(ctx context.Context, opt Options) error {
	if opt.Tags == nil {
		return fmt.Errorf("tag lister is required")
	}
	if opt.HealthTimeout == 0 {
		opt.HealthTimeout = 10 * time.Minute
	}
	if opt.PollEvery == 0 {
		opt.PollEvery = 2 * time.Second
	}

	docs, err := load(ctx, opt)
	if err != nil {
		return err
	}
	if len(docs) == 0 {
		return fmt.Errorf("nothing to scan: pass --compose-file and/or --portainer-url")
	}

	var (
		decisions []Decision
		failed    bool
	)
	for _, d := range docs {
		for _, svc := range d.file.Services {
			dec := decide(ctx, opt, d.src, svc)
			decisions = append(decisions, dec)
			printDecision(opt, dec)
			if dec.Err != nil {
				failed = true
			}
		}
	}

	if opt.Mode == Plan {
		if failed {
			return fmt.Errorf("plan failed for one or more services")
		}
		return nil
	}

	// Group apply by document so each file/stack is written once.
	type key struct {
		kind, name, path string
		id               int
	}
	groups := map[key][]Decision{}
	order := []key{}
	for _, dec := range decisions {
		if dec.Err != nil || !dec.Changed {
			continue
		}
		k := key{kind: dec.Source.Kind, name: dec.Source.Name, path: dec.Source.Path}
		if dec.Source.HasStack {
			k.id = dec.Source.Stack.ID
		}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], dec)
	}

	if len(order) == 0 && !failed {
		fmt.Fprintln(opt.out(), "no pin changes")
		return nil
	}

	for _, k := range order {
		group := groups[k]
		src := group[0].Source
		text := documentText(docs, src)
		for _, dec := range group {
			var err error
			text, err = compose.RewriteImage(text, dec.Service, dec.To)
			if err != nil {
				fmt.Fprintf(opt.errw(), "error: rewrite %s/%s: %v\n", src.Name, dec.Service.Name, err)
				failed = true
				text = ""
				break
			}
		}
		if text == "" {
			continue
		}
		if err := applyDoc(ctx, opt, src, text, group); err != nil {
			fmt.Fprintf(opt.errw(), "error: apply %s: %v\n", src.Name, err)
			failed = true
			// No rollback — the pin may already be written.
			continue
		}
	}

	if failed {
		return fmt.Errorf("apply failed for one or more services")
	}
	return nil
}

type loaded struct {
	src  Source
	file compose.File
}

func load(ctx context.Context, opt Options) ([]loaded, error) {
	var out []loaded
	for _, path := range opt.ComposeFiles {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		f, err := compose.Parse(string(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		src := Source{Kind: "local", Name: path, Path: path, HasHealth: map[string]bool{}}
		for _, s := range f.Services {
			src.HasHealth[s.Name] = s.HasHealthcheck
		}
		out = append(out, loaded{src: src, file: f})
	}
	if opt.Portainer == nil {
		return out, nil
	}
	stacks, err := opt.Portainer.ListStacks(ctx)
	if err != nil {
		return nil, fmt.Errorf("list portainer stacks: %w", err)
	}
	for _, st := range stacks {
		if !matchStack(opt.StackFilter, st.Name) {
			continue
		}
		if st.Type != portainer.TypeCompose && st.Type != portainer.TypeSwarm {
			fmt.Fprintf(opt.errw(), "skip portainer/%s: unsupported type %d\n", st.Name, st.Type)
			continue
		}
		if st.GitConfig != nil && st.GitConfig.URL != "" {
			fmt.Fprintf(opt.errw(), "skip portainer/%s: git-backed stack (PUT would detach git)\n", st.Name)
			continue
		}
		text, err := opt.Portainer.StackFile(ctx, st.ID)
		if err != nil {
			return nil, fmt.Errorf("portainer stack %s file: %w", st.Name, err)
		}
		f, err := compose.Parse(text)
		if err != nil {
			return nil, fmt.Errorf("portainer stack %s: %w", st.Name, err)
		}
		src := Source{
			Kind:      "portainer",
			Name:      st.Name,
			Stack:     st,
			HasStack:  true,
			HasHealth: map[string]bool{},
		}
		for _, s := range f.Services {
			src.HasHealth[s.Name] = s.HasHealthcheck
		}
		out = append(out, loaded{src: src, file: f})
	}
	return out, nil
}

func matchStack(filter []string, name string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, f := range filter {
		if strings.EqualFold(f, name) {
			return true
		}
	}
	return false
}

func decide(ctx context.Context, opt Options, src Source, svc compose.Service) Decision {
	dec := Decision{Source: src, Service: svc, From: svc.Image.Tag}
	tags, err := opt.Tags.ListTags(ctx, svc.Image)
	if err != nil {
		dec.Err = err
		dec.Reason = "list tags"
		return dec
	}
	newest, changed := svc.Selector.Choose(svc.Image.Tag, tags)
	if newest == "" {
		dec.Reason = "no allowed tag"
		return dec
	}
	dec.To = newest
	dec.NewRef = svc.Image.WithTag(newest)
	dec.Changed = changed
	if !changed {
		dec.Reason = "already newest allowed"
	}
	return dec
}

func printDecision(opt Options, dec Decision) {
	loc := dec.Source.Kind + "/" + dec.Source.Name
	switch {
	case dec.Err != nil:
		fmt.Fprintf(opt.errw(), "ERROR  %s  %s  %v\n", loc, dec.Service.Name, dec.Err)
	case dec.Changed:
		fmt.Fprintf(opt.out(), "BUMP   %s  %s  %s -> %s\n", loc, dec.Service.Name, dec.From, dec.To)
	default:
		reason := dec.Reason
		if reason == "" {
			reason = "noop"
		}
		fmt.Fprintf(opt.out(), "NOOP   %s  %s  %s (%s)\n", loc, dec.Service.Name, dec.From, reason)
	}
}

func documentText(docs []loaded, src Source) string {
	for _, d := range docs {
		if d.src.Kind == src.Kind && d.src.Name == src.Name && d.src.Path == src.Path {
			return d.file.Text
		}
	}
	return ""
}

func applyDoc(ctx context.Context, opt Options, src Source, text string, group []Decision) error {
	switch src.Kind {
	case "local":
		if err := os.WriteFile(src.Path, []byte(text), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(opt.out(), "wrote  %s\n", src.Path)
		if opt.SkipDeploy {
			return nil
		}
		if opt.Deploy == nil {
			return fmt.Errorf("local apply needs docker compose (or pass --skip-deploy)")
		}
		var names []string
		for _, d := range group {
			names = append(names, d.Service.Name)
		}
		if err := opt.Deploy.Up(ctx, src.Path, names); err != nil {
			return fmt.Errorf("docker compose up: %w", err)
		}
		return waitLocal(ctx, opt, src, group)
	case "portainer":
		if opt.Portainer == nil {
			return fmt.Errorf("portainer client missing")
		}
		if err := opt.Portainer.UpdateStack(ctx, src.Stack, text, opt.PullImage); err != nil {
			return err
		}
		fmt.Fprintf(opt.out(), "updated portainer stack %s (id %d)\n", src.Stack.Name, src.Stack.ID)
		return waitPortainer(ctx, opt, src, group)
	default:
		return fmt.Errorf("unknown source %s", src.Kind)
	}
}

func waitLocal(ctx context.Context, opt Options, src Source, group []Decision) error {
	var need []Decision
	for _, d := range group {
		if d.Service.HasHealthcheck {
			need = append(need, d)
		}
	}
	if len(need) == 0 {
		return nil
	}
	return poll(ctx, opt, func(ctx context.Context) (bool, error) {
		for _, d := range need {
			h, err := opt.Deploy.Health(ctx, src.Path, d.Service.Name)
			if err != nil {
				return false, err
			}
			if done, err := healthOutcome(h); err != nil || !done {
				return done, err
			}
		}
		return true, nil
	})
}

func waitPortainer(ctx context.Context, opt Options, src Source, group []Decision) error {
	var need []Decision
	for _, d := range group {
		if d.Service.HasHealthcheck {
			need = append(need, d)
		}
	}
	if len(need) == 0 || opt.Portainer == nil {
		return nil
	}
	return poll(ctx, opt, func(ctx context.Context) (bool, error) {
		ctrs, err := opt.Portainer.ListContainers(ctx, src.Stack.EndpointID)
		if err != nil {
			return false, err
		}
		for _, d := range need {
			var match *portainer.Container
			for i := range ctrs {
				if portainer.MatchesStack(ctrs[i], src.Stack.Name, d.Service.Name) {
					match = &ctrs[i]
					break
				}
			}
			if match == nil {
				return false, nil
			}
			ins, err := opt.Portainer.InspectContainer(ctx, src.Stack.EndpointID, match.ID)
			if err != nil {
				return false, err
			}
			h := Health{
				Found:    true,
				Running:  ins.State.Running,
				State:    ins.State.Status,
				ExitCode: ins.State.ExitCode,
			}
			if ins.State.Health != nil {
				h.HasCheck = true
				h.Status = ins.State.Health.Status
			}
			if done, err := healthOutcome(h); err != nil || !done {
				return done, err
			}
		}
		return true, nil
	})
}

func healthOutcome(h Health) (done bool, err error) {
	if !h.Found {
		return false, nil
	}
	if !h.Running && strings.EqualFold(h.State, "exited") {
		return true, fmt.Errorf("container exited (exit %d); no rollback", h.ExitCode)
	}
	if !h.HasCheck {
		if h.Running {
			return true, nil
		}
		return false, nil
	}
	switch strings.ToLower(h.Status) {
	case "healthy":
		return true, nil
	case "unhealthy":
		return true, fmt.Errorf("healthcheck unhealthy; no rollback")
	default:
		return false, nil
	}
}

func poll(ctx context.Context, opt Options, fn func(context.Context) (bool, error)) error {
	deadline := time.Now().Add(opt.HealthTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		done, err := fn(ctx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for health after %s; no rollback", opt.HealthTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(opt.PollEvery):
		}
	}
}
