// Small progressive enhancement for server-rendered filter forms.
// The visible submit button remains available when JavaScript is disabled.
document.addEventListener("change", function (event) {
  var control = event.target.closest("select[data-auto-submit]");
  if (control && control.form) {
    if (control.name === "kind") {
      var status = control.form.querySelector('select[name="status"]');
      if (status) status.value = "";
    }
    control.form.requestSubmit();
  }
});

// Copy is an enhancement only: the preformatted value remains selectable when
// Clipboard API access is unavailable (for example on an insecure origin).
document.addEventListener("click", async function (event) {
  var button = event.target.closest("[data-copy-target]");
  if (!button) return;

  var target = document.getElementById(button.dataset.copyTarget);
  if (!target) return;

  var original = button.textContent;
  var copied = false;
  try {
    await navigator.clipboard.writeText(target.textContent);
    copied = true;
  } catch (_) {
    var selection = window.getSelection();
    var range = document.createRange();
    range.selectNodeContents(target);
    selection.removeAllRanges();
    selection.addRange(range);
    copied = document.execCommand("copy");
    selection.removeAllRanges();
  }

  button.textContent = copied ? "已复制" : "请手动复制";
  var status = document.getElementById("ui-status");
  if (status) status.textContent = copied ? "内容已复制到剪贴板" : "自动复制失败，请手动选择内容复制";
  window.setTimeout(function () { button.textContent = original; }, 1800);
});
