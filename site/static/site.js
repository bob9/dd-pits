// Scroll reveals. Content is visible without JS; the .js class opts in to the
// animation, and anything already on screen is revealed immediately.
(function () {
  var root = document.documentElement;
  if (!("IntersectionObserver" in window)) return;
  if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) return;
  root.classList.add("js");
  var io = new IntersectionObserver(function (entries) {
    entries.forEach(function (e) {
      if (e.isIntersecting) {
        e.target.classList.add("in");
        io.unobserve(e.target);
      }
    });
  }, { rootMargin: "0px 0px -8% 0px", threshold: 0.08 });
  function start() {
    document.querySelectorAll(".reveal").forEach(function (el) { io.observe(el); });
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start);
  else start();
})();

// Highlight the download for this visitor's computer.
(function () {
  var ua = navigator.userAgent, p = (navigator.userAgentData && navigator.userAgentData.platform) || navigator.platform || "";
  var os = /Win/i.test(p + ua) ? "windows" : /Mac/i.test(p + ua) ? "mac" : /Linux/i.test(p + ua) ? (/arm|aarch/i.test(ua) ? "linux-arm" : "linux") : "";
  if (!os) return;
  var el = document.querySelector('.dl-btn[data-os="' + os + '"]');
  if (el) el.classList.add("mine");
})();

// See it in action: the RotorHazard / FPVTrackside switch over the video.
(function () {
  var tabs = document.querySelectorAll('.watch-switch button');
  if (!tabs.length) return;
  var vids = { rh: document.getElementById('watchRH'), ft: document.getElementById('watchFT') };
  tabs.forEach(function (tab) {
    tab.addEventListener('click', function () {
      tabs.forEach(function (t) { t.setAttribute('aria-selected', t === tab ? 'true' : 'false'); });
      Object.keys(vids).forEach(function (k) {
        if (!vids[k]) return;
        if (k === tab.dataset.v) { vids[k].hidden = false; }
        else { vids[k].pause(); vids[k].hidden = true; }
      });
    });
  });
})();

// Grouped menu: one group open at a time; close on a click elsewhere or on a link.
(function () {
  var groups = document.querySelectorAll('.nav .ng, .nav .nav-mobile');
  if (!groups.length) return;
  groups.forEach(function (g) {
    g.addEventListener('toggle', function () {
      if (!g.open) return;
      groups.forEach(function (o) { if (o !== g && !o.contains(g) && !g.contains(o)) o.open = false; });
    });
  });
  document.addEventListener('click', function (e) {
    groups.forEach(function (g) { if (g.open && !g.contains(e.target)) g.open = false; });
  });
  document.querySelectorAll('.nav .ng-menu a, .nav .nav-top').forEach(function (a) {
    a.addEventListener('click', function () { groups.forEach(function (g) { g.open = false; }); });
  });
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') groups.forEach(function (g) { g.open = false; });
  });
})();
