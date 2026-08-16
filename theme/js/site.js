/* context-guru docs — dependency-free progressive enhancement.
 *
 * The theme is already applied pre-paint by an inline <head> script; this file
 * wires the toggle, the mobile nav, code copy buttons, TOC scrollspy, the search
 * overlay, and mermaid theming. Everything here is additive: with JS off the page
 * is still fully readable and navigable (the only loss is search and diagrams).
 */
(function () {
  "use strict";

  var root = document.documentElement;
  var reduce = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  var base = window.CG_BASE || ".";

  /* ── theme toggle ─────────────────────────────────────────────── */

  var THEME_KEY = "cg-theme";
  var toggle = document.querySelector(".theme-toggle");

  function currentTheme() {
    return root.getAttribute("data-theme") === "light" ? "light" : "dark";
  }

  function setTheme(next) {
    root.setAttribute("data-theme", next);
    try { localStorage.setItem(THEME_KEY, next); } catch (e) { /* private mode */ }
    if (toggle) {
      toggle.setAttribute("aria-label", next === "light" ? "Switch to dark theme" : "Switch to light theme");
    }
    // Mermaid bakes its colours into the SVG it emits, so a theme flip has to
    // re-render every diagram rather than just recolour the page.
    renderMermaid();
  }

  if (toggle) {
    toggle.setAttribute("aria-label", currentTheme() === "light" ? "Switch to dark theme" : "Switch to light theme");
    toggle.addEventListener("click", function () {
      setTheme(currentTheme() === "light" ? "dark" : "light");
    });
  }

  /* ── mobile nav drawer ────────────────────────────────────────── */

  var burger = document.getElementById("nav-burger");
  var navLinks = document.getElementById("nav-links");
  if (burger && navLinks) {
    burger.addEventListener("click", function () {
      var open = navLinks.classList.toggle("open");
      burger.setAttribute("aria-expanded", String(open));
      burger.setAttribute("aria-label", open ? "Close navigation" : "Open navigation");
    });
  }

  /* ── code copy buttons ────────────────────────────────────────── */

  if (navigator.clipboard) {
    document.querySelectorAll("main pre").forEach(function (pre) {
      if (pre.classList.contains("mermaid") || pre.closest(".linenos")) return;
      var code = pre.querySelector("code") || pre;
      var btn = document.createElement("button");
      btn.className = "copy-btn";
      btn.type = "button";
      btn.textContent = "copy";
      btn.setAttribute("aria-label", "Copy code to clipboard");
      btn.addEventListener("click", function () {
        navigator.clipboard.writeText(code.innerText.replace(/\n$/, "")).then(function () {
          btn.textContent = "copied";
          btn.classList.add("done");
          setTimeout(function () { btn.textContent = "copy"; btn.classList.remove("done"); }, 1400);
        });
      });
      pre.appendChild(btn);
    });
  }

  /* ── scroll-reveal ────────────────────────────────────────────── */

  var reveals = document.querySelectorAll(".reveal");
  if (reduce || !("IntersectionObserver" in window)) {
    reveals.forEach(function (el) { el.classList.add("in"); });
  } else {
    var io = new IntersectionObserver(function (entries) {
      entries.forEach(function (e) {
        if (e.isIntersecting) { e.target.classList.add("in"); io.unobserve(e.target); }
      });
    }, { rootMargin: "0px 0px -8% 0px", threshold: 0.08 });
    reveals.forEach(function (el) { io.observe(el); });
  }

  /* ── TOC scrollspy ────────────────────────────────────────────── */

  var tocLinks = Array.prototype.slice.call(document.querySelectorAll(".toc a[href^='#']"));
  if (tocLinks.length && "IntersectionObserver" in window) {
    var byId = {};
    var targets = [];
    tocLinks.forEach(function (a) {
      var id = decodeURIComponent(a.getAttribute("href").slice(1));
      var el = document.getElementById(id);
      if (el) { byId[el.id] = a; targets.push(el); }
    });
    var spy = new IntersectionObserver(function (entries) {
      entries.forEach(function (e) {
        if (!e.isIntersecting) return;
        tocLinks.forEach(function (a) { a.classList.remove("active"); });
        var link = byId[e.target.id];
        if (link) link.classList.add("active");
      });
    }, { rootMargin: "-15% 0px -70% 0px", threshold: 0 });
    targets.forEach(function (t) { spy.observe(t); });
  }

  /* ── search ───────────────────────────────────────────────────────
   * MkDocs' search plugin writes search/search_index.json (title + full text
   * per page and per section). That is all we need: scoring 55 pages by term
   * frequency in the browser is instant, so there is no lunr.js bundle here and
   * no index shipped twice. Loaded lazily on first open.
   * ponytail: substring/term scoring, no stemming — swap in lunr only if a real
   * query stops finding a page it should. */

  var overlay = document.getElementById("search-overlay");
  var input = document.getElementById("search-input");
  var results = document.getElementById("search-results");
  var openBtn = document.getElementById("search-open");
  var closeBtn = document.getElementById("search-close");
  var docsIndex = null;
  var indexState = "idle";
  var lastFocus = null;

  function loadIndex() {
    if (indexState !== "idle") return Promise.resolve(docsIndex);
    indexState = "loading";
    return fetch(base.replace(/\/$/, "") + "/search/search_index.json")
      .then(function (r) { return r.json(); })
      .then(function (data) {
        // Keep only page-level records (no "#anchor" in location): a hit list of
        // 400 section fragments buries the page the reader actually wants.
        docsIndex = (data.docs || []).filter(function (d) {
          return d.location && d.location.indexOf("#") === -1 && d.title;
        }).map(function (d) {
          return {
            location: d.location,
            title: d.title,
            text: (d.text || "").replace(/\s+/g, " "),
            hay: (d.title + " " + d.location + " " + (d.text || "")).toLowerCase()
          };
        });
        indexState = "ready";
        return docsIndex;
      })
      .catch(function () { indexState = "error"; return null; });
  }

  function escapeHtml(s) {
    return s.replace(/[&<>"]/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c];
    });
  }

  function snippet(text, term) {
    var i = text.toLowerCase().indexOf(term);
    if (i === -1) return escapeHtml(text.slice(0, 140)) + "…";
    var from = Math.max(0, i - 50);
    var slice = text.slice(from, from + 170);
    var html = escapeHtml(slice);
    var re = new RegExp("(" + term.replace(/[.*+?^${}()|[\]\\]/g, "\\$&") + ")", "gi");
    return (from > 0 ? "…" : "") + html.replace(re, "<mark>$1</mark>") + "…";
  }

  function render(query) {
    var terms = query.toLowerCase().split(/\s+/).filter(Boolean);
    if (!docsIndex || !terms.length) {
      results.innerHTML = '<p class="search-empty">Type to search every page on the site.</p>';
      return;
    }
    var hits = [];
    docsIndex.forEach(function (d) {
      var score = 0;
      for (var i = 0; i < terms.length; i++) {
        var t = terms[i];
        if (d.hay.indexOf(t) === -1) return;           // every term must appear
        if (d.title.toLowerCase().indexOf(t) !== -1) score += 12;
        if (d.location.toLowerCase().indexOf(t) !== -1) score += 6;
        score += Math.min(8, d.text.toLowerCase().split(t).length - 1);
      }
      hits.push({ d: d, score: score });
    });
    hits.sort(function (a, b) { return b.score - a.score; });
    if (!hits.length) {
      results.innerHTML = '<p class="search-empty">No page matches “' + escapeHtml(query) + '”.</p>';
      return;
    }
    results.innerHTML = hits.slice(0, 20).map(function (h) {
      var crumb = h.d.location.replace(/\/$/, "").split("/").slice(0, -1).join(" / ") || "top level";
      return '<a class="search-hit" href="' + base.replace(/\/$/, "") + "/" + h.d.location + '" role="option">' +
        '<div class="search-hit-crumb">' + escapeHtml(crumb) + "</div>" +
        '<div class="search-hit-title">' + escapeHtml(h.d.title) + "</div>" +
        '<div class="search-hit-text">' + snippet(h.d.text, terms[0]) + "</div></a>";
    }).join("");
  }

  function openSearch() {
    if (!overlay) return;
    lastFocus = document.activeElement;
    overlay.hidden = false;
    input.focus();
    input.select();
    loadIndex().then(function () { render(input.value); });
  }

  function closeSearch() {
    if (!overlay || overlay.hidden) return;
    overlay.hidden = true;
    if (lastFocus && lastFocus.focus) lastFocus.focus();
  }

  if (overlay && input && results) {
    if (openBtn) openBtn.addEventListener("click", openSearch);
    if (closeBtn) closeBtn.addEventListener("click", closeSearch);
    overlay.addEventListener("click", function (e) { if (e.target === overlay) closeSearch(); });
    input.addEventListener("input", function () { render(input.value); });

    document.addEventListener("keydown", function (e) {
      var typing = /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement.tagName);
      if (e.key === "Escape") { closeSearch(); return; }
      if ((e.key === "k" || e.key === "K") && (e.metaKey || e.ctrlKey)) { e.preventDefault(); openSearch(); return; }
      if (e.key === "/" && !typing && overlay.hidden) { e.preventDefault(); openSearch(); }
    });

    // ↑/↓/Enter through the hit list.
    input.addEventListener("keydown", function (e) {
      var hits = Array.prototype.slice.call(results.querySelectorAll(".search-hit"));
      if (!hits.length) return;
      var i = hits.findIndex(function (h) { return h.classList.contains("sel"); });
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        if (i >= 0) hits[i].classList.remove("sel");
        var next = e.key === "ArrowDown" ? (i + 1) % hits.length : (i <= 0 ? hits.length - 1 : i - 1);
        hits[next].classList.add("sel");
        hits[next].scrollIntoView({ block: "nearest" });
      } else if (e.key === "Enter") {
        e.preventDefault();
        (i >= 0 ? hits[i] : hits[0]).click();
      }
    });
  }

  /* ── mermaid ──────────────────────────────────────────────────────
   * Diagrams come from ```mermaid fences, so the source lives in the Markdown
   * where the prose is. We render them ourselves rather than letting mermaid
   * auto-start, for two reasons: the colours have to be read off the *current*
   * theme's custom properties, and a theme flip has to re-render from the
   * original source (mermaid bakes colours into the SVG). */

  var mermaidNodes = Array.prototype.slice.call(document.querySelectorAll("pre.mermaid, .mermaid"));

  // Stash the source before the first render replaces the node's contents.
  mermaidNodes.forEach(function (el) {
    if (!el.dataset.src) el.dataset.src = el.textContent.trim();
  });

  function cssVar(name) {
    return getComputedStyle(root).getPropertyValue(name).trim();
  }

  function mermaidTheme() {
    var ink = cssVar("--ink");
    var line = cssVar("--sub");
    return {
      startOnLoad: false,
      securityLevel: "strict",
      theme: "base",
      fontFamily: '"Fira Sans", Inter, system-ui, sans-serif',
      themeVariables: {
        darkMode: currentTheme() === "dark",
        background: cssVar("--bg-soft"),
        primaryColor: cssVar("--bg-2"),
        primaryTextColor: ink,
        primaryBorderColor: cssVar("--accent"),
        secondaryColor: cssVar("--bg-soft"),
        secondaryTextColor: ink,
        secondaryBorderColor: cssVar("--border"),
        tertiaryColor: cssVar("--bg-soft"),
        tertiaryTextColor: ink,
        tertiaryBorderColor: cssVar("--border"),
        lineColor: line,
        textColor: ink,
        mainBkg: cssVar("--bg-2"),
        nodeBorder: cssVar("--accent"),
        clusterBkg: cssVar("--bg-soft"),
        clusterBorder: cssVar("--border"),
        edgeLabelBackground: cssVar("--bg-soft"),
        titleColor: ink,
        noteBkgColor: cssVar("--amber-soft"),
        noteTextColor: ink,
        noteBorderColor: cssVar("--amber"),
        actorBkg: cssVar("--bg-2"),
        actorBorder: cssVar("--accent"),
        actorTextColor: ink,
        signalColor: line,
        signalTextColor: ink,
        labelBoxBkgColor: cssVar("--bg-2"),
        labelBoxBorderColor: cssVar("--border"),
        labelTextColor: ink,
        loopTextColor: ink,
        activationBkgColor: cssVar("--accent-soft"),
        activationBorderColor: cssVar("--accent"),
        sequenceNumberColor: cssVar("--bg")
      }
    };
  }

  var mermaidSeq = 0;

  function renderMermaid() {
    var mermaid = window.cgMermaid;
    if (!mermaid || !mermaidNodes.length) return;
    mermaid.initialize(mermaidTheme());
    mermaidNodes.forEach(function (el) {
      var src = el.dataset.src || "";
      if (!src) return;
      var id = "cg-mmd-" + (mermaidSeq++);
      mermaid.render(id, src).then(function (out) {
        el.innerHTML = out.svg;
        el.dataset.rendered = "ok";
      }).catch(function (err) {
        // Never leave a rendered "Syntax error in text" box on the page: fall
        // back to the readable source and put the reason in the console.
        el.dataset.rendered = "error";
        el.innerHTML = "<code></code>";
        el.firstChild.textContent = src;
        // Mermaid appends its own error SVG to <body> on failure; drop it.
        var orphan = document.getElementById("d" + id) || document.getElementById(id);
        if (orphan && orphan.parentNode === document.body) orphan.remove();
        console.error("mermaid failed to render a diagram on this page:", err);
      });
    });
  }

  window.cgRenderMermaid = renderMermaid;
})();
