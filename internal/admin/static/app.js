// Action buttons (toggle/delete/backup-now) are plain <button data-action="...">,
// not forms — clicking one POSTs to data-action via fetch and updates the DOM
// per data-ajax, instead of navigating. The multi-field forms (add database,
// add schedule, add storage target) are untouched: they submit normally
// because they need a fresh page anyway.
document.addEventListener("click", async (e) => {
  const btn = e.target.closest("button[data-action]");
  if (!btn) return;

  const confirmMsg = btn.getAttribute("data-confirm");
  if (confirmMsg && !confirm(confirmMsg)) return;

  const url = btn.getAttribute("data-action");
  btn.disabled = true;

  try {
    const res = await fetch(url, { method: "POST" });
    if (!res.ok) {
      throw new Error((await res.text()).trim() || res.statusText);
    }
    applyAction(btn);
  } catch (err) {
    alert("Lỗi: " + err.message);
    btn.disabled = false;
  }
});

function applyAction(btn) {
  const action = btn.getAttribute("data-ajax");
  const row = btn.closest("tr");

  if (action === "remove-row") {
    row?.remove();
    return;
  }

  if (action === "toggle-enabled") {
    const badge = row?.querySelector(".badge");
    const wasEnabled = badge?.classList.contains("on");
    if (badge) {
      badge.classList.toggle("on", !wasEnabled);
      badge.classList.toggle("off", wasEnabled);
      badge.textContent = wasEnabled ? "Tắt" : "Bật";
    }
    setLabel(btn, wasEnabled ? "Bật" : "Tắt");
    btn.disabled = false;
    return;
  }

  if (action === "reload") {
    location.reload();
    return;
  }

  if (action === "flash" || action === "check-connection") {
    const original = labelEl(btn).textContent;
    setLabel(btn, action === "flash" ? "Đã đẩy ✓" : "Kết nối OK ✓");
    setTimeout(() => {
      setLabel(btn, original);
      btn.disabled = false;
    }, 2000);
    return;
  }

  btn.disabled = false;
}

// The button's icon is an <svg> sibling of a .label span holding the text —
// updating textContent on the button itself would wipe the icon out along
// with the old text, so text changes always go through this span instead.
function labelEl(btn) {
  return btn.querySelector(".label") || btn;
}

function setLabel(btn, text) {
  labelEl(btn).textContent = text;
}

// Highlight the topbar nav link for the current page. Sub-pages (edit
// forms, "new" pages, a database's file list, ...) don't have their own nav
// entry — matching by href prefix instead of exact path covers those too,
// falling back to "/" (Databases) for anything no other link's prefix
// claims, since every uncovered route today is database-related.
const navLinks = document.querySelectorAll(".topbar nav a");
if (navLinks.length) {
  const path = location.pathname;
  let active = document.querySelector('.topbar nav a[href="/"]');
  navLinks.forEach((a) => {
    const href = a.getAttribute("href");
    if (href !== "/" && path.startsWith(href)) active = a;
  });
  active?.classList.add("active");
}

// Logs page "Chỉ hiện lỗi" button — pure client-side row filter, the page
// already has every row loaded so there's no need for a server round-trip.
const logsFilterBtn = document.getElementById("logs-filter-error");
if (logsFilterBtn) {
  const rows = document.querySelectorAll("table tbody tr[data-status]");
  let errorsOnly = false;
  logsFilterBtn.addEventListener("click", () => {
    errorsOnly = !errorsOnly;
    rows.forEach((row) => {
      row.style.display = errorsOnly && row.dataset.status === "success" ? "none" : "";
    });
    logsFilterBtn.classList.toggle("primary", errorsOnly);
    setLabel(logsFilterBtn, errorsOnly ? "Hiện tất cả" : "Chỉ hiện lỗi");
  });
}

// Auth DB only applies to mongo — the form ships it pre-hidden/shown for the
// current driver (server-rendered, avoids a flash of the wrong state), this
// just keeps it in sync as the user changes the driver dropdown.
const driverSelect = document.getElementById("driver-select");
if (driverSelect) {
  const mongoOnlyFields = document.querySelectorAll("[data-mongo-only]");
  driverSelect.addEventListener("change", () => {
    const isMongo = driverSelect.value === "mongo";
    mongoOnlyFields.forEach((el) => {
      el.style.display = isMongo ? "" : "none";
    });
  });
}
