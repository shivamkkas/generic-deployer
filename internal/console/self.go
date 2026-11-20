package console

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// selfUnit is the systemd unit this console runs in ("" when started by hand),
// read from /proc/self/cgroup, e.g. "0::/system.slice/generic-deploy.service".
var selfUnit = func() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		parts := strings.Split(line, "/")
		if last := parts[len(parts)-1]; strings.HasSuffix(last, ".service") {
			return last
		}
	}
	return ""
}()

func unitName(u string) string {
	if strings.Contains(u, ".") {
		return u
	}
	return u + ".service"
}

func isSelf(unit string) bool { return selfUnit != "" && unitName(unit) == selfUnit }

// selfAction handles actions on the console's own unit. It returns true when
// the request was fully handled here.
//   - stop: refused, it would leave nothing to start it again from.
//   - restart: answered first, then done ~2s later so the reply reaches the
//     browser before the process is replaced (the page reconnects by itself).
func (s *Server) selfAction(w http.ResponseWriter, r *http.Request, t *Target, a Action) bool {
	switch a.Name {
	case "stop":
		http.Error(w, "this is the deploy console's own service; stopping it here would leave no UI to start it again. Use a shell: systemctl stop "+selfUnit, http.StatusConflict)
		return true
	case "restart":
		who := userFrom(r).Name
		log.Printf("audit: user=%s ip=%s target=%s action=restart (self, in 2s)", who, r.RemoteAddr, t.Name)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "$ %s\n\nThis is the deploy console itself: it restarts in 2 seconds.\nThe page reconnects automatically; you stay signed in.\n\n[exit 0 after 0s]\n", strings.Join(a.argv, " "))
		go func() {
			time.Sleep(2 * time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if out, err := t.cmd(ctx, []string{"systemctl", "restart", "--no-block", t.Unit}).CombinedOutput(); err != nil {
				log.Printf("self-restart failed: %v %s", err, out)
			}
		}()
		return true
	}
	return false
}
