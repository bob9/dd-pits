/* dd-pits on RotorHazard's Event page.
 *
 * 1. Heat groups as sections. RotorHazard lists a grouped class's heats in
 *    one block, each titled "Round N / <heat>". This lays them out round by
 *    round under a heading per round, in round order. Display only: the
 *    heats and groups themselves are RotorHazard's own.
 * 2. #dd-heat-<id> (a link from a dd-pits bracket) scrolls to that heat and
 *    highlights it.
 * 3. A class that is a dd-pits bracket gets a link to the bracket.
 */
(function () {
  var list = document.getElementById('heat_list');
  if (!list) return;
  var busy = false;
  var focus = null; // {name, round} from /dd-pits/api/heat-info
  var brackets = {}; // class id -> {stage, url} from /dd-pits/api/class-links

  function bracketLinks() {
    list.querySelectorAll('li[data-class-id]').forEach(function (li) {
      var b = brackets[li.getAttribute('data-class-id')];
      var h = li.querySelector(':scope > h3');
      if (!b || !h || h.querySelector('.dd-bracket-link')) return;
      var a = document.createElement('a');
      a.className = 'dd-bracket-link';
      a.href = b.url;
      a.textContent = '\uD83C\uDFC1 Open the bracket in dd-pits \u2192';
      a.style.cssText = 'margin-left:.8em;font-size:.7em;font-weight:600;padding:.2em .6em;border-radius:999px;' +
        'color:#c6ff3d;background:rgba(0,0,0,.3);border:1px solid rgba(198,255,61,.55);text-decoration:none;vertical-align:middle';
      h.appendChild(a);
    });
  }

  function roundOf(li) {
    var t = li.querySelector('h4,h3,h2');
    var m = t && /^\s*Round\s+(\d+)\s*\/\s*/.exec(t.textContent);
    return m ? parseInt(m[1], 10) : null;
  }

  function sections() {
    list.querySelectorAll('ol.heats').forEach(function (ol) {
      var heats = Array.prototype.filter.call(ol.children, function (li) {
        return li.classList.contains('heat');
      });
      if (!heats.length || heats.some(function (li) { return roundOf(li) === null; })) return;
      var key = heats.map(function (li) { return roundOf(li) + ':' + li.textContent.length; }).join(',');
      if (ol.dataset.ddRounds === key) return; // already laid out
      heats.sort(function (a, b) { return roundOf(a) - roundOf(b); }); // stable
      ol.querySelectorAll('.dd-round-head').forEach(function (h) { h.remove(); });
      var last = null;
      heats.forEach(function (li) {
        var r = roundOf(li);
        if (r !== last) {
          var h = document.createElement('li');
          h.className = 'dd-round-head';
          h.textContent = 'Round ' + r;
          h.style.cssText = 'grid-column:1/-1;flex-basis:100%;width:100%;list-style:none;margin:1.1em 0 .3em;' +
            'padding:.35em .6em;font-weight:700;font-size:1.1em;border-left:4px solid #c6ff3d;background:rgba(198,255,61,.08)';
          ol.appendChild(h);
          last = r;
        }
        ol.appendChild(li);
      });
      ol.dataset.ddRounds = key;
    });
  }

  function highlight() {
    if (!focus) return false;
    var hit = null;
    list.querySelectorAll('li.heat').forEach(function (li) {
      var t = li.querySelector('h4,h3,h2');
      if (!t || hit) return;
      var text = t.textContent.trim();
      var name = text.replace(/^\s*Round\s+\d+\s*\/\s*/, '');
      if (name === focus.name && (focus.round === null || roundOf(li) === null || roundOf(li) === focus.round)) hit = li;
    });
    if (!hit) return false;
    hit.style.outline = '3px solid #c6ff3d';
    hit.style.borderRadius = '8px';
    hit.style.background = 'rgba(198,255,61,.12)';
    if (!focus.scrolled) { hit.scrollIntoView({ behavior: 'smooth', block: 'center' }); focus.scrolled = true; }
    return true;
  }

  function apply() {
    if (busy) return;
    busy = true;
    try { sections(); highlight(); bracketLinks(); } finally { busy = false; }
  }

  new MutationObserver(function () { setTimeout(apply, 0); }).observe(list, { childList: true, subtree: true });
  apply();

  fetch('/dd-pits/api/class-links').then(function (r) { return r.json(); }).then(function (d) {
    brackets = d || {};
    apply();
  }).catch(function () {});

  var m = /dd-heat-(\d+)/.exec(location.hash);
  if (m) {
    fetch('/dd-pits/api/heat-info?heat=' + m[1]).then(function (r) { return r.json(); }).then(function (d) {
      if (!d || !d.name) return;
      focus = { name: d.name, round: d.round || null };
      apply();
    }).catch(function () {});
  }
})();
