package plugui

import (
	"html/template"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ModelEntry is one row of a model catalogue.
//
// Native is the model name the upstream provider itself uses, and it is what the
// catalogue displays. ID is the name this deployment routes requests by; it can
// differ from Native in two ways that must not change the listing:
//
//   - the plugin renames some upstream ids so one model does not appear twice
//     under two spellings (each of those plugins documents its own mapping);
//   - the host may prefix a model with its account when `model_prefix` is on.
//
// Neither is a fact about the upstream catalogue, so neither belongs in a list
// whose purpose is to show what the provider offers. ID is still carried, because
// the routing name is what a reader copies into a request; it is shown only when
// it differs from Native.
type ModelEntry struct {
	// Native is the provider's own model name. Required; blank rows are dropped.
	Native string
	// ID is the name requests are routed by. Empty means "same as Native".
	ID string
	// Detail is free-form extra context (context window, modalities, …).
	Detail string
}

// ModelCatalogue is the shared 「模型目录」 card: a summary, a filter box, and the
// provider's own model names.
//
// Every plugin renders this card, so the one question it exists to answer —
// "which models does this channel offer?" — has an answer on every page.
type ModelCatalogue struct {
	// Source describes where the listing came from (one short line).
	Source string
	// Entries are the models to list. Duplicates by Native are dropped.
	Entries []ModelEntry
	// Actions are appended to the card's action row, typically 刷新目录.
	Actions []Action
	// EmptyNotice is shown instead of the list when there is nothing to list.
	EmptyNotice string
}

// filterThreshold is the entry count above which the filter box is rendered.
//
// Below it a filter costs the reader more than it saves: one more control to
// skip past in order to read a list that already fits on one screen. The box
// appears only where it was asked for — lists long enough to need scrolling.
const filterThreshold = 12

// CatalogueCard renders the shared 「模型目录」 card.
func CatalogueCard(catalogue ModelCatalogue) template.HTML {
	rows := make([]catalogueRow, 0, len(catalogue.Entries))
	seen := make(map[string]bool, len(catalogue.Entries))
	for _, entry := range catalogue.Entries {
		native := strings.TrimSpace(entry.Native)
		if native == "" {
			continue
		}
		// Rows are keyed by the ROUTING id, which is what makes two entries
		// distinct models. Keying on the displayed name instead merges genuinely
		// different models that the provider happens to label the same way —
		// measured on Cline's live catalogue, where `qwen/qwen3.8-27b` and
		// `qwen/qwen3.8-27b:free` share one display name and 12 rows vanished.
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			id = native
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		routed := ""
		if id != native {
			routed = id
		}
		rows = append(rows, catalogueRow{
			Native: native,
			Routed: routed,
			Detail: strings.TrimSpace(entry.Detail),
			// SearchText carries every spelling a reader might type, so a filter
			// on the routed name matches even though that name is not the label.
			SearchText: strings.ToLower(native + " " + id),
		})
	}

	body := render(catalogueBodyTmpl, struct {
		Source      string
		Rows        []catalogueRow
		Total       int
		ShowFilter  bool
		EmptyNotice string
	}{
		Source:      catalogue.Source,
		Rows:        rows,
		Total:       len(rows),
		ShowFilter:  len(rows) > filterThreshold,
		EmptyNotice: catalogue.EmptyNotice,
	})
	return Card("模型目录", body, catalogue.Actions...)
}

// catalogueRow is one rendered model row.
type catalogueRow struct {
	Native string
	// Routed is the routing name when it differs from Native, else empty.
	Routed string
	Detail string
	// SearchText is the lower-cased haystack the filter matches against.
	SearchText string
}

// catalogueBodyTmpl renders the card BODY; Card supplies the <section> wrapper
// and the action row, so this must not repeat them.
//
// The filter is plain JavaScript with no dependency and no network call: it
// toggles rows that were already rendered. Verified to execute inside
// CPA-Manager-Plus's plugin iframe (same origin, no `sandbox` attribute, no
// Content-Security-Policy), and the page stays fully readable without it — the
// input simply does nothing and every row is already visible.
var catalogueBodyTmpl = template.Must(template.New("catalogueBody").Parse(
	`{{if .Source}}<dl class="fields"><dt>来源</dt><dd>{{.Source}}</dd><dt>数量</dt><dd>{{.Total}}</dd></dl>{{end}}` +
		`{{if .Rows}}` +
		`{{if .ShowFilter}}<div class="cat-filter">` +
		`<input type="search" id="cat-filter-input" placeholder="筛选模型（{{.Total}} 个，输入名称片段）" autocomplete="off" spellcheck="false">` +
		`<span class="muted" id="cat-filter-count"></span></div>{{end}}` +
		`<table class="cat-table" id="cat-table"><tbody>` +
		`{{range .Rows}}<tr data-search="{{.SearchText}}"><td>` +
		`<span class="mono">{{.Native}}</span>` +
		`{{if .Routed}}<span class="cat-routed">请求用名 <span class="mono">{{.Routed}}</span></span>{{end}}` +
		`{{if .Detail}}<span class="cat-detail muted">{{.Detail}}</span>{{end}}` +
		`</td></tr>{{end}}` +
		`</tbody></table>` +
		`<p class="cat-none muted" id="cat-none" hidden>没有匹配的模型。</p>` +
		`{{else}}<p class="muted">{{.EmptyNotice}}</p>{{end}}` +
		`{{if .ShowFilter}}<script>
(function () {
  var input = document.getElementById('cat-filter-input');
  var table = document.getElementById('cat-table');
  var none = document.getElementById('cat-none');
  var count = document.getElementById('cat-filter-count');
  if (!input || !table) return;
  var rows = Array.prototype.slice.call(table.querySelectorAll('tbody tr'));
  function apply() {
    var needle = input.value.trim().toLowerCase();
    var shown = 0;
    for (var i = 0; i < rows.length; i++) {
      var haystack = rows[i].getAttribute('data-search') || '';
      var hit = !needle || haystack.indexOf(needle) !== -1;
      rows[i].hidden = !hit;
      if (hit) shown++;
    }
    if (none) none.hidden = shown !== 0;
    if (count) count.textContent = needle ? ('匹配 ' + shown + ' / ' + rows.length) : '';
  }
  input.addEventListener('input', apply);
  input.addEventListener('search', apply);
  apply();
})();
</script>{{end}}`))

// ModelEntriesFromInfo builds catalogue rows from the host-facing model
// descriptors a plugin already produces for `model.for_auth`, so the page and
// the published catalogue cannot disagree.
//
// It reads Name (the provider-native name) as the displayed value and ID as the
// routing name. A descriptor whose Name is empty falls back to ID, so a plugin
// that never set Name still lists its models rather than dropping every row.
//
// detail, when non-nil, returns the trailing per-row context for one descriptor.
func ModelEntriesFromInfo(infos []pluginapi.ModelInfo, detail func(pluginapi.ModelInfo) string) []ModelEntry {
	entries := make([]ModelEntry, 0, len(infos))
	for _, info := range infos {
		native := strings.TrimSpace(info.Name)
		if native == "" {
			native = strings.TrimSpace(info.ID)
		}
		entry := ModelEntry{Native: native, ID: strings.TrimSpace(info.ID)}
		if detail != nil {
			entry.Detail = detail(info)
		}
		entries = append(entries, entry)
	}
	return entries
}
