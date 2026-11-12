/* shell/table-links: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("shell/table-links",null,["document"],function(deps,lifecycle){
'use strict';
/* oao v1.3.4 encodes an entire {url} as a path component. Use its documented
 * table-loaded hook for whole-value links until that dependency gains a URL
 * renderer. Parameter templates keep their normal encodeURIComponent behavior.
 * No HTML decoding and no global fetch/renderer patching. */
'use strict';
lifecycle.listen(deps.document,'oao:table-loaded', function (event) {
    var detail = event.detail;
    if (!detail || !detail.meta || !detail.el) return;
    var columns = detail.meta.columns.filter(function (col) { return !col.hidden; });
    columns.forEach(function (col, index) {
        if (col.render !== 'link' || col.href !== '{' + col.name + '}') return;
        detail.el.querySelectorAll('tbody tr').forEach(function (row) {
            var cell = row.children[index], link = cell && cell.querySelector('a.oao-link');
            if (!link) return;
            // v1.3.4 uses the field's full text for both display and binding.
            var value = link.textContent.trim();
            try {
                var url = new URL(value);
                if (!/^https?:$/.test(url.protocol) || !/^https?:\/\//i.test(value) || /[\u0000-\u0020\u007f]/.test(value)) throw new Error('unsafe URL');
                link.setAttribute('href', url.href);
                if (col.new_tab) link.setAttribute('rel', 'noopener noreferrer');
            } catch (_) {
                link.removeAttribute('href');
                link.setAttribute('title', '非 HTTP(S) 地址，不提供跳转');
            }
        });
    });
});



return Object.defineProperties({}, {

});
});
MonitorShell.install("shell/table-links");
