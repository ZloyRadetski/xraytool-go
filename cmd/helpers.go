package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"time"

	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
	"xraytool/internal/slave"
	"xraytool/internal/user"
)

// ---------------------------------------------------------------------------
// Printer — dual-mode output (batch vs interactive)
// ---------------------------------------------------------------------------

// Printer handles output in two modes:
//   - Batch mode (machine-readable): "ERROR|msg" / "SUCCESS|field1|field2"
//   - Interactive mode: ANSI-coloured human-readable text
type Printer struct {
	Batch bool
}

func newPrinter(batch bool) *Printer { return &Printer{Batch: batch} }

// Error prints an error message and returns it as an error object.
func (p *Printer) Error(msg string) error {
	if p.Batch {
		fmt.Println("ERROR|" + msg)
	} else {
		fmt.Fprintf(os.Stderr, "\n\033[0;31m[ERROR] %s\033[0m\n", msg)
	}
	return fmt.Errorf("command failed: %s", msg)
}

// Errorf is Error with printf formatting.
func (p *Printer) Errorf(format string, args ...interface{}) error {
	return p.Error(fmt.Sprintf(format, args...))
}

// Success prints a success message (batch only; interactive callers print inline).
func (p *Printer) Success(fields ...string) {
	if p.Batch {
		out := "SUCCESS"
		for _, f := range fields {
			out += "|" + f
		}
		fmt.Println(out)
	}
}

// Info prints an informational message (interactive only).
func (p *Printer) Info(format string, args ...interface{}) {
	if !p.Batch {
		fmt.Printf("\n\033[0;34m[INFO] "+format+"\033[0m\n", args...)
	}
}

// OK prints a success message (interactive only).
func (p *Printer) OK(format string, args ...interface{}) {
	if !p.Batch {
		fmt.Printf("\n\033[0;32m[OK] "+format+"\033[0m\n", args...)
	}
}

// Warn prints a warning (interactive only).
func (p *Printer) Warn(format string, args ...interface{}) {
	if !p.Batch {
		fmt.Printf("\n\033[1;33m[WARN] "+format+"\033[0m\n", args...)
	}
}

// Println prints a plain line regardless of mode.
func (p *Printer) Println(s string) {
	fmt.Println(s)
}

// ---------------------------------------------------------------------------
// Validation helpers
// ---------------------------------------------------------------------------

var validEmailRe = regexp.MustCompile(`^[a-zA-Z0-9@._][a-zA-Z0-9@._-]*$`)

// validEmail returns true if the email contains only allowed characters.
func validEmail(email string) bool {
	return validEmailRe.MatchString(email)
}

// ---------------------------------------------------------------------------
// Date helpers
// ---------------------------------------------------------------------------

// defaultExpireDate returns a date 30 days from now in DD-MM-YYYY format.
func defaultExpireDate() string {
	return time.Now().AddDate(0, 0, 30).Format("02-01-2006")
}

// ---------------------------------------------------------------------------
// Slave propagation
// ---------------------------------------------------------------------------

// propagate sends a command to all slave servers in parallel and prints results.
// In batch mode, errors are silently swallowed (callers log separately).
func propagate(cfg *appconfig.Config, cmd string, params map[string]string, print *Printer) {
	if !cfg.IsMaster() {
		return
	}

	reg := slaveRegistry(cfg)
	results := reg.PropagateAll(cmd, params)
	for _, r := range results {
		if r.Err != nil {
			if !print.Batch {
				fmt.Fprintf(os.Stderr, "  [slave:%s] FAIL: %v\n", r.Server, r.Err)
			}
		} else {
			if !print.Batch {
				fmt.Printf("  [slave:%s] OK\n", r.Server)
			}
		}
	}
}

// slaveRegistry builds a slave.Registry from the current config.
func slaveRegistry(cfg *appconfig.Config) *slave.Registry {
	c := slave.NewClient(
		cfg.SlaveAPI.ConnectTimeout,
		cfg.SlaveAPI.RequestTimeout,
		cfg.SlaveAPI.RemotePath,
	)
	return slave.NewRegistry(cfg.SlaveServers, c)
}

// ---------------------------------------------------------------------------
// System helpers
// ---------------------------------------------------------------------------

// systemctlRestart restarts a systemd service. Non-fatal on failure.
var systemctlRestart = func(service string) {
	fmt.Printf("INFO|Restarting systemd service: %s\n", service)
	err := exec.Command("systemctl", "restart", service).Run()
	if err != nil {
		fmt.Printf("WARN|Failed to restart service %s: %v\n", service, err)
	} else {
		fmt.Printf("INFO|Successfully restarted service %s\n", service)
	}
}

// subfileID strips the ".txt" suffix from a subfile name for use in URLs.
func subfileID(subfile string) string {
	if len(subfile) > 4 && subfile[len(subfile)-4:] == ".txt" {
		return subfile[:len(subfile)-4]
	}
	return subfile
}

// resolveEmail handles the duplicated logic of selecting an email from flags, interactive menu,
// or stdin, and validates it. It calls p.Error (which prints error) if validation fails.
func resolveEmail(email, emailAlias string, allowMenu bool, promptText string, engine domain.Engine, p *Printer) (string, error) {
	if email == "" {
		email = emailAlias
	}

	if email == "" && !p.Batch {
		if allowMenu {
			email = selectUserInteractive(engine, p)
		} else {
			fmt.Print(promptText)
			fmt.Scanln(&email)
		}
	}

	if email == "" {
		return "", p.Error("email is required")
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9@._-]+$`).MatchString(email) {
		return "", p.Error("invalid email format")
	}
	return email, nil
}

// limitPtrFromStr parses a string into a *float64.
// Returns nil if the string is empty.
func limitPtrFromStr(s string) (*float64, error) {
	return user.ParseLimit(s)
}
