// Keyboard shortcuts for the dashboard. Press ? for the list.
(function () {
  "use strict";
  var help = [
    ["/", "Search"], ["j / k", "Next / previous issue"], ["o or Enter", "Open the selected issue"],
    ["x", "Select the issue for a bulk action"], ["g then h", "All projects"], ["g then s", "This project's settings"],
    ["?", "Show this list"], ["Esc", "Close, or leave the search box"],
  ];
  var pending = null, selected = -1;

  function typing(e) {
    var t = e.target;
    return t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName));
  }
  function rows() { return Array.prototype.slice.call(document.querySelectorAll("ul.issues > li")); }
  function select(i) {
    var list = rows();
    if (!list.length) return;
    selected = Math.max(0, Math.min(list.length - 1, i));
    list.forEach(function (li, n) { li.classList.toggle("selected", n === selected); });
    var link = list[selected].querySelector("a");
    if (link) link.focus({ preventScroll: true });
    list[selected].scrollIntoView({ block: "nearest" });
  }
  function project() {
    var m = location.pathname.match(/^\/p\/([^/]+)/);
    return m && m[1];
  }
  function toggleHelp() {
    var existing = document.getElementById("shortcut-help");
    if (existing) { existing.close(); existing.remove(); return; }
    var d = document.createElement("dialog");
    d.id = "shortcut-help";
    d.setAttribute("aria-label", "Keyboard shortcuts");
    var h = document.createElement("h2");
    h.textContent = "Keyboard shortcuts";
    var table = document.createElement("table");
    table.className = "context";
    help.forEach(function (row) {
      var tr = document.createElement("tr"), th = document.createElement("th"), td = document.createElement("td");
      th.appendChild(document.createElement("kbd")).textContent = row[0];
      td.textContent = row[1];
      tr.appendChild(th); tr.appendChild(td); table.appendChild(tr);
    });
    d.appendChild(h); d.appendChild(table);
    d.addEventListener("close", function () { d.remove(); });
    document.body.appendChild(d);
    d.showModal();
  }

  document.addEventListener("keydown", function (e) {
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    if (typing(e)) {
      if (e.key === "Escape") e.target.blur();
      return;
    }
    var key = e.key;
    if (pending === "g") {
      pending = null;
      if (key === "h") { location.href = "/"; e.preventDefault(); }
      else if (key === "s" && project()) { location.href = "/p/" + project() + "/settings"; e.preventDefault(); }
      return;
    }
    switch (key) {
      case "/":
        var search = document.querySelector("[data-shortcut='/']") || document.querySelector("form.search input");
        if (search) { search.focus(); search.select(); e.preventDefault(); }
        break;
      case "j": select(selected + 1); e.preventDefault(); break;
      case "k": select(selected - 1); e.preventDefault(); break;
      case "o":
      case "Enter":
        var list = rows();
        if (selected >= 0 && list[selected] && key === "o") { list[selected].querySelector("a").click(); e.preventDefault(); }
        break;
      case "x":
        var row = rows()[selected];
        var box = row && row.querySelector("input[type=checkbox]");
        if (box) { box.checked = !box.checked; e.preventDefault(); }
        break;
      case "g": pending = "g"; setTimeout(function () { pending = null; }, 1200); break;
      case "?": toggleHelp(); e.preventDefault(); break;
    }
  });
})();
