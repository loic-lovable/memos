// Command provision demonstrates explicit, persisted single-account decisions.
// Use it only with an enrolled disposable application and a private job parent.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/client"
)

type options struct {
	profile, job, action, authority, source, subject, revision string
	name, department, email, clear                             string
	supplied                                                   map[string]bool
}

func parseOptions(args []string, stderr io.Writer) (options, error) {
	var o options
	f := flag.NewFlagSet("provision", flag.ContinueOnError)
	f.SetOutput(stderr)
	f.StringVar(&o.profile, "profile", "", "enrolled client JSON file")
	f.StringVar(&o.job, "job", "", "one private job directory per explicit decision")
	f.StringVar(&o.action, "action", "", "create, read, update, activate, disable, recover or retry")
	f.StringVar(&o.authority, "authority", "", "expected enrolled authority for a new decision")
	f.StringVar(&o.source, "source", "", "stable source reference for creation")
	f.StringVar(&o.subject, "subject", "", "exact subject ID for read or an existing-account decision")
	f.StringVar(&o.revision, "revision", "", "previously observed subject revision for a new existing-account decision")
	f.StringVar(&o.name, "display-name", "", "exact display name (an explicitly empty value is allowed)")
	f.StringVar(&o.department, "department", "", "exact department")
	f.StringVar(&o.email, "email", "", "legacy scalar contact email; never an account link")
	f.StringVar(&o.clear, "clear", "", "comma-separated scalar fields to clear")
	if err := f.Parse(args); err != nil {
		return o, err
	}
	if f.NArg() != 0 || o.profile == "" {
		return o, errors.New("provide -profile and named flags only")
	}
	o.supplied = map[string]bool{}
	f.Visit(func(f *flag.Flag) { o.supplied[f.Name] = true })
	allowed := map[string]bool{"profile": true, "action": true}
	permit := func(names ...string) {
		for _, name := range names {
			allowed[name] = true
		}
	}
	switch o.action {
	case "recover", "retry":
		permit("job")
	case "read":
		permit("subject")
		if o.subject == "" {
			return o, errors.New("read requires -subject")
		}
	case "create":
		permit("job", "authority", "source", "display-name", "department", "email")
		if o.authority == "" || o.source == "" || !o.supplied["display-name"] {
			return o, errors.New("create requires -authority, -source and -display-name")
		}
	case "update", "activate", "disable":
		permit("job", "authority", "subject", "revision")
		if o.authority == "" || o.subject == "" || o.revision == "" {
			return o, errors.New("existing-account decisions require -authority, -subject and the observed -revision")
		}
		if o.action == "update" {
			permit("display-name", "department", "email", "clear")
			if !o.supplied["display-name"] && !o.supplied["department"] && !o.supplied["email"] && o.clear == "" {
				return o, errors.New("update requires a supplied field or -clear")
			}
		}
	default:
		return o, errors.New("choose -action create, read, update, activate, disable, recover or retry")
	}
	if o.action != "read" && o.job == "" {
		return o, errors.New("provide -job under an existing private directory")
	}
	for name := range o.supplied {
		if !allowed[name] {
			return o, fmt.Errorf("-%s is not accepted for %s; saved jobs never acquire new arguments", name, o.action)
		}
	}
	return o, nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	ctx, timeout := context.WithTimeout(ctx, 60*time.Second)
	defer timeout()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	o, err := parseOptions(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	profile, err := client.ReadProfile(o.profile)
	if err != nil {
		return err
	}
	peer, err := client.New(profile)
	if err != nil {
		return err
	}
	defer peer.Close()
	if o.action == "read" {
		if err := peer.Authenticate(ctx); err != nil {
			return err
		}
		if _, err := peer.Discover(ctx); err != nil {
			return err
		}
		record, err := peer.ReadHuman(ctx, o.subject)
		if err != nil {
			return err
		}
		// Reading is an explicit observation, never automatic revision rebasing.
		return json.NewEncoder(stdout).Encode(map[string]any{"subject": record.ID, "revision": record.Revision, "authority": record.Authority, "lifecycle": record.Lifecycle})
	}
	resume := o.action == "recover" || o.action == "retry"
	j, err := openJob(o.job, !resume)
	if err != nil {
		return err
	}
	defer j.close()
	var intent client.Intent
	if resume {
		raw, err := j.load()
		if err != nil {
			return err
		}
		intent, err = peer.RestoreIntent(raw)
		if err != nil {
			return err
		}
	}
	// Recovery uses read authority and never performs discovery or a mutation.
	if o.action == "recover" {
		if err := peer.Authenticate(ctx); err != nil {
			return err
		}
		outcome, err := peer.RecoverTyped(ctx, intent)
		return report(stdout, intent, outcome, err)
	}
	if err := peer.AuthenticateWrite(ctx); err != nil {
		return err
	}
	if !resume {
		if _, err := peer.Discover(ctx); err != nil {
			return err
		}
		intent, err = prepare(ctx, peer, o)
		if err != nil {
			return err
		}
	}
	outcome, err := peer.SubmitTyped(ctx, intent, j.save)
	return report(stdout, intent, outcome, err)
}

func prepare(ctx context.Context, peer *client.Client, o options) (client.Intent, error) {
	version := client.SubjectVersion{ID: o.subject, Revision: o.revision, Authority: o.authority}
	switch o.action {
	case "create":
		human := client.Human{Authority: o.authority, SourceReference: o.source, DisplayName: o.name}
		if o.supplied["department"] {
			human.Department = &o.department
		}
		if o.supplied["email"] {
			human.Email = &o.email
		}
		return peer.PrepareCreate(ctx, human)
	case "update":
		changes := client.ScalarChanges{Set: map[string]string{}}
		for flag, pair := range map[string][2]string{"display-name": {"displayName", o.name}, "department": {"department", o.department}, "email": {"email", o.email}} {
			if o.supplied[flag] {
				changes.Set[pair[0]] = pair[1]
			}
		}
		if o.clear != "" {
			changes.Clear = strings.Split(o.clear, ",")
		}
		return peer.PrepareUpdate(ctx, version, changes)
	case "activate":
		return peer.PrepareActivate(ctx, version)
	case "disable":
		return peer.PrepareDisable(ctx, version)
	default:
		return client.Intent{}, errors.New("not a new provisioning decision")
	}
}

func report(out io.Writer, intent client.Intent, outcome client.Outcome, callErr error) error {
	summary := map[string]any{"outcome": outcome.State(), "operation_id": intent.ID(), "replay_window": intent.ReplayWindow()}
	if receipt := outcome.Receipt; receipt != nil {
		summary["commit"] = receipt.Commit.State
		summary["resources"] = receipt.Commit.Resources
		summary["poll_after_seconds"] = receipt.PollAfterSeconds
		if receipt.Error != nil {
			summary["error_code"] = receipt.Error.Code
		}
	}
	if err := json.NewEncoder(out).Encode(summary); err != nil {
		return err
	}
	if callErr != nil {
		return fmt.Errorf("outcome unavailable; keep the saved job and recover it: %w", callErr)
	}
	if outcome.State() != client.OutcomeSucceeded {
		return fmt.Errorf("operation outcome is %s; preserve this job and inspect its receipt before another decision", outcome.State())
	}
	return nil
}
