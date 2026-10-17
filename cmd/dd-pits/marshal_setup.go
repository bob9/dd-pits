package main

import (
	"fmt"
	"strings"
)

// rhTimerCard: the RotorHazard behind FPVTrackside, for marshalling from its
// signal traces. Shown on the FPVTrackside setup page.
func (s *liveServer) rhTimerCard(b *strings.Builder) {
	t := s.cfg.RHTimerConfig()
	detected := detectFTRotorHazard()
	url, key := ftRHTimer(s.cfg)
	status := `<p class="mut">No RotorHazard timer found in FPVTrackside's timer settings. If FPVTrackside times with a RotorHazard, enter its address.</p>`
	if url != "" {
		switch {
		case pluginAnswers(url):
			status = fmt.Sprintf(`<p class="ok">Marshalling reads signal traces from <b>%s</b> through the dd-pits plugin there.</p>`, esc(url))
			if key == "" {
				status += `<p class="warn">Enter the plugin's key (shown on that RotorHazard under Settings → dd-pits) so dd-pits may read and save.</p>`
			}
		default:
			if sio, err := dialSIO(url); err == nil {
				sio.close()
				status = fmt.Sprintf(`<p class="ok">Marshalling reads signal traces from <b>%s</b> through the FPVTrackside connector (it needs connector 1.2.0 or later; installing the dd-pits plugin there works with any).</p>`, esc(url))
			} else {
				status = fmt.Sprintf(`<p class="warn">Can't reach the RotorHazard at %s: %s</p>`, esc(url), esc(err.Error()))
			}
		}
	}
	note := ""
	if detected != "" {
		note = fmt.Sprintf(` <span class="mut">(FPVTrackside uses %s)</span>`, esc(detected))
	}
	fmt.Fprintf(b, `<div class="card"><h2>RotorHazard timer: marshalling</h2>
<p class="mut">When FPVTrackside times with a RotorHazard, dd-pits' Marshal page works from each pilot's signal trace on that RotorHazard: auto-detected enter/exit lines, laps recalculated, odd laps flagged. Corrections are saved in RotorHazard and reach FPVTrackside through its connector.</p>
%s<form method="post" class="row" style="align-items:flex-end;margin-top:.6rem">
<input type="hidden" name="rh_form" value="1">
<div><label>Address%s</label><input type="text" name="rh_url" value="%s" placeholder="%s"></div>
<div><label>dd-pits plugin key (if installed there)</label><input type="text" name="rh_key" value="%s"></div>
<div style="flex:0"><button>Save</button></div></form></div>`,
		status, note, esc(t.URL), esc(firstNonEmpty(detected, "http://192.168.1.50:5000")), esc(t.Key))
}
