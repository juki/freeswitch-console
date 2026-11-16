// Applies the colour theme before first paint. A classic script on purpose:
// module scripts are deferred, which would flash the wrong theme.
(function () {
  var mode = 'auto';
  try {
    mode = localStorage.getItem('fsconsole-theme') || 'auto';
  } catch (e) {
    // storage disabled: stay on auto
  }
  var mq = window.matchMedia('(prefers-color-scheme: dark)');

  function apply() {
    var dark = mode === 'dark' || (mode === 'auto' && mq.matches);
    document.documentElement.dataset.theme = dark ? 'dark' : 'light';
  }

  apply();
  mq.addEventListener('change', apply);

  window.fsconsoleTheme = {
    get: function () {
      return mode;
    },
    set: function (m) {
      mode = m;
      try {
        localStorage.setItem('fsconsole-theme', m);
      } catch (e) {
        // not persisted
      }
      apply();
    },
  };
})();
