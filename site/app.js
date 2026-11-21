// Theme: follow the system until the visitor picks one.
(function () {
  var root = document.documentElement;
  function stored() {
    try { return localStorage.getItem("theme"); } catch (e) { return null; }
  }
  var saved = stored();
  if (saved === "light" || saved === "dark") root.dataset.theme = saved;

  document.addEventListener("click", function (e) {
    var btn = e.target.closest("[data-theme-toggle]");
    if (!btn) return;
    var dark = root.dataset.theme
      ? root.dataset.theme === "dark"
      : window.matchMedia("(prefers-color-scheme: dark)").matches;
    root.dataset.theme = dark ? "light" : "dark";
    try { localStorage.setItem("theme", root.dataset.theme); } catch (err) {}
  });
})();

function esc(s) {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

// Small highlighters for the two languages the site shows.
function yaml(src) {
  return src.split("\n").map(function (line) {
    var comment = "";
    var m = line.match(/^(.*?)(\s*#.*)$/);
    if (m && (m[1].match(/"/g) || []).length % 2 === 0) { line = m[1]; comment = m[2]; }
    var out = esc(line)
      .replace(/"[^"]*"/g, '<span class="s">$&</span>')
      .replace(/^(\s*-?\s*)([A-Za-z_][\w.-]*)(:)/, '$1<span class="k">$2</span>$3');
    return out + (comment ? '<span class="c">' + esc(comment) + "</span>" : "");
  }).join("\n");
}

function shell(src) {
  return src.split("\n").map(function (line) {
    if (/^\s*#/.test(line)) return '<span class="c">' + esc(line) + "</span>";
    var m = line.match(/^(\$ )(.*?)(\s+#.*)?$/);
    if (m) return '<span class="p">$ </span>' + esc(m[2]) + (m[3] ? '<span class="c">' + esc(m[3]) + "</span>" : "");
    return '<span class="d">' + esc(line) + "</span>";
  }).join("\n");
}

document.querySelectorAll("pre[data-lang]").forEach(function (pre) {
  var src = pre.textContent.replace(/^\n/, "").replace(/\s+$/, "");
  pre.dataset.src = src;
  pre.innerHTML = pre.dataset.lang === "yaml" ? yaml(src) : pre.dataset.lang === "sh" ? shell(src) : esc(src);
});

// Copy buttons copy the command without the prompt, or the whole block.
document.addEventListener("click", function (e) {
  var btn = e.target.closest(".copy");
  if (!btn) return;
  var text = btn.dataset.copy;
  if (!text) {
    var pre = btn.closest(".window, .demo-main").querySelector("pre");
    text = (pre.dataset.src || pre.textContent)
      .split("\n").filter(function (l) { return !/^\s*#/.test(l) || pre.dataset.lang !== "sh"; })
      .map(function (l) { return l.replace(/^\$ /, "").replace(/\s+#.*$/, pre.dataset.lang === "sh" ? "" : "$&"); })
      .join("\n");
  }
  if (!navigator.clipboard) return;
  navigator.clipboard.writeText(text).then(function () {
    btn.classList.add("done");
    setTimeout(function () { btn.classList.remove("done"); }, 1400);
  });
});

// The plan demo: the same rules hydra applies, on a four-service stack.
(function () {
  var out = document.getElementById("plan-out");
  if (!out) return;

  var SLOT = 1, STRIDE = 200, HOST = "127.0.0.1";
  var services = {
    api: { port: 8080, paths: ["services/api/", "packages/shared/"], upstreams: {} },
    db: { shared: true, paths: ["db/migrations/"] },
    deps: { follow: true },
    web: { port: 3000, paths: ["apps/web/", "packages/shared/"], needs: ["deps"],
           upstreams: { API_URL: "api", JOBS_QUEUE: "worker", NEXT_PUBLIC_SITE_URL: "web" } },
    worker: { paths: ["services/worker/", "packages/shared/"], url: "jobs-{slot}", baselineUrl: "jobs",
              upstreams: { API_URL: "api" } }
  };
  var names = Object.keys(services).sort();

  function value(name, local) {
    var s = services[name];
    if (!local && s.baselineUrl) return s.baselineUrl;
    var slot = local ? SLOT : 0;
    return (s.url || "http://{host}:{port}")
      .replace("{host}", HOST).replace("{port}", s.port + slot * STRIDE).replace("{slot}", slot);
  }

  function pad(rows) {
    var widths = [];
    rows.forEach(function (r) { r.forEach(function (c, i) { widths[i] = Math.max(widths[i] || 0, c.length); }); });
    return rows.map(function (r) {
      return r.map(function (c, i) { return i === r.length - 1 ? c : c + " ".repeat(widths[i] - c.length); }).join("  ");
    });
  }

  function render() {
    var changed = [];
    document.querySelectorAll(".file[aria-pressed='true']").forEach(function (b) { changed.push(b.dataset.path); });
    changed.sort();

    var local = {}, why = {}, warnings = [];
    names.forEach(function (n) {
      var s = services[n];
      var hit = (s.paths || []).length && changed.filter(function (f) {
        return s.paths.some(function (p) { return f.indexOf(p) === 0; }) && !/_test\.go$/.test(f);
      })[0];
      if (s.shared) {
        why[n] = hit ? "shared, always baseline (changed: " + hit + ")" : "shared";
        if (hit) warnings.push("warning: " + n + " is shared, but its paths changed: " + hit + "; the baseline's copy is used");
      } else if (hit) {
        local[n] = true; why[n] = "changed: " + hit;
      } else {
        why[n] = "unchanged";
      }
    });
    names.forEach(function (n) {
      if (!services[n].follow) return;
      var by = names.filter(function (m) { return local[m] && (services[m].needs || []).indexOf(n) >= 0; })[0];
      if (by) { local[n] = true; why[n] = "needed by " + by; }
    });

    var rows = pad([["SERVICE", "RUNS", "PORT", "WHY"]].concat(names.map(function (n) {
      var s = services[n];
      return [n, local[n] ? "here" : "baseline", s.port ? String(s.port + (local[n] ? SLOT * STRIDE : 0)) : "-", why[n]];
    })));

    var wires = [];
    names.forEach(function (n) {
      if (!local[n]) return;
      Object.keys(services[n].upstreams || {}).sort().forEach(function (env) {
        var t = services[n].upstreams[env];
        wires.push([n, env, "-> " + t + " (" + (local[t] ? "local" : "baseline") + ")", value(t, !!local[t]),
          "default: " + t + (local[t] ? " running here" : " not running here")]);
      });
    });

    var html = '<span class="p">$ </span>hydra plan\n';
    html += '<span class="d">worktree, slot 1: ~/code/shop-checkout</span>\n\n';
    html += '<span class="d">' + esc(rows[0]) + "</span>\n";
    rows.slice(1).forEach(function (r, i) {
      var n = names[i];
      html += (local[n] ? '<span class="x">' + esc(r) + "</span>" : /^.*shared, always/.test(r) ? '<span class="w">' + esc(r) + "</span>" : esc(r)) + "\n";
    });
    html += '\n<span class="d">wiring:</span>\n';
    if (wires.length) {
      pad(wires).forEach(function (w) { html += "  " + esc(w) + "\n"; });
    } else {
      var any = names.some(function (n) { return local[n]; });
      html += '<span class="d">  ' + (any ? "(what runs here points at no other service)" : "(nothing runs here)") + "</span>\n";
    }
    if (warnings.length) html += "\n" + warnings.map(function (w) { return '<span class="w">' + esc(w) + "</span>"; }).join("\n") + "\n";
    out.innerHTML = html;
  }

  document.querySelectorAll(".file").forEach(function (b) {
    b.addEventListener("click", function () {
      b.setAttribute("aria-pressed", b.getAttribute("aria-pressed") === "true" ? "false" : "true");
      render();
    });
  });
  render();
})();

// Docs sidebar: mark the section in view.
(function () {
  var links = document.querySelectorAll(".side a[href^='#']");
  if (!links.length || !("IntersectionObserver" in window)) return;
  var byId = {};
  links.forEach(function (a) { byId[a.getAttribute("href").slice(1)] = a; });
  var visible = new Set();
  var io = new IntersectionObserver(function (entries) {
    entries.forEach(function (en) { en.isIntersecting ? visible.add(en.target.id) : visible.delete(en.target.id); });
    var first = null;
    links.forEach(function (a) {
      var id = a.getAttribute("href").slice(1);
      if (!first && visible.has(id)) first = id;
    });
    if (first) links.forEach(function (a) { a.classList.toggle("active", a === byId[first]); });
  }, { rootMargin: "-80px 0px -55% 0px" });
  Object.keys(byId).forEach(function (id) {
    var el = document.getElementById(id);
    if (el) io.observe(el);
  });
})();
