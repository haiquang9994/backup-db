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

  if (action === "flash") {
    const original = labelEl(btn).textContent;
    setLabel(btn, "Đã đẩy ✓");
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
