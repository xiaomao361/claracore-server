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
