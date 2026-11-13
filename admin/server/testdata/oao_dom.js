// Minimal eventful DOM for exercising the real bundled oao, including its
// private confirmation handlers. It models no layout or browser networking.
function makeDOM() {
    const doc = { activeElement: null, listeners: {}, parentNode: null };
    const decode = value => value.replace(/&(?:amp|lt|gt|quot|#39);/g, value => ({'&amp;':'&','&lt;':'<','&gt;':'>','&quot;':'"','&#39;':"'"}[value]));
    function matches(el, selector) {
        const parts = selector.trim().split(/\s+/);
        function simple(node, selector) {
            if (!node || !node.tagName) return false;
            const negatives = [...selector.matchAll(/:not\(([^)]+)\)/g)].map(match => match[1]);
            selector = selector.replace(/:not\([^)]+\)/g, '');
            if (negatives.some(value => simple(node, value))) return false;
            const attrs = [...selector.matchAll(/\[([^=\]]+)(?:=["']?([^\]"']*)["']?)?\]/g)];
            selector = selector.replace(/\[[^\]]+\]/g, '');
            if (attrs.some(match => !node.hasAttribute(match[1]) || (match[2] !== undefined && node.getAttribute(match[1]) !== match[2]))) return false;
            const tag = selector.match(/^[\w-]+/), id = selector.match(/#([\w-]+)/);
            return (!tag || node.tagName === tag[0].toUpperCase()) && (!id || node.id === id[1]) && [...selector.matchAll(/\.([\w-]+)/g)].every(match => node.classList.contains(match[1]));
        }
        if (!simple(el, parts.pop())) return false;
        let at = el.parentNode;
        while (parts.length) { const part = parts.pop(); while (at && !simple(at, part)) at = at.parentNode; if (!at) return false; at = at.parentNode; }
        return true;
    }
    function listen(type, fn) { (this.listeners[type] ||= []).push(fn); }
    function dispatch(event) {
        event.target ||= this; event.preventDefault ||= () => {}; event.stopPropagation ||= () => { event.stopped = true; };
        for (let at = this; at; at = at.parentNode) {
            if (at['on' + event.type]) at['on' + event.type](event);
            for (const listener of at.listeners[event.type] || []) listener(event);
            if (event.stopped) break;
        }
    }
    class Element {
        constructor(tag) {
            this.tagName = tag.toUpperCase(); this.attrs = {}; this.children = []; this.parentNode = null; this.listeners = {}; this.style = {}; this.dataset = {}; this._text = ''; this._html = ''; this.value = '';
            this.classList = {
                contains: value => this.className.split(/\s+/).includes(value),
                add: value => { if (!this.classList.contains(value)) this.className += ' ' + value; },
                remove: value => { this.className = this.className.split(/\s+/).filter(item => item !== value).join(' '); },
                toggle: (value, on) => this.classList[on ? 'add' : 'remove'](value)
            };
        }
        get id() { return this.attrs.id || ''; } set id(value) { this.attrs.id = value; }
        get className() { return this.attrs.class || ''; } set className(value) { this.attrs.class = value; }
        get hidden() { return this.hasAttribute('hidden'); } set hidden(value) { value ? this.setAttribute('hidden', '') : this.removeAttribute('hidden'); }
        get isConnected() { return this === doc.body || !!(this.parentNode && this.parentNode.isConnected); }
        get textContent() { return this._text + this.children.map(child => child.textContent).join(''); }
        set textContent(value) { this.replaceChildren(); this._html = ''; this._text = String(value); }
        get innerHTML() { return this._html; }
        set innerHTML(value) {
            this.replaceChildren(); this._text = ''; this._html = String(value); const stack = [this];
            for (const token of String(value).match(/<[^>]+>|[^<]+/g) || []) {
                if (token.startsWith('</')) { if (stack.length > 1) stack.pop(); continue; }
                if (!token.startsWith('<')) { stack.at(-1)._text += decode(token); continue; }
                const tag = token.match(/^<([\w-]+)/); if (!tag) continue; const el = new Element(tag[1]);
                for (const attr of token.slice(tag[0].length, -1).matchAll(/([^\s=]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s]+)))?/g)) el.setAttribute(attr[1], decode(attr[2] ?? attr[3] ?? attr[4] ?? ''));
                stack.at(-1).appendChild(el); if (!/^(input|img|br|hr|meta|link)$/i.test(tag[1])) stack.push(el);
            }
        }
        setAttribute(key, value) { this.attrs[key] = String(value); if (key === 'value') this.value = String(value); }
        getAttribute(key) { return this.attrs[key] ?? null; }
        hasAttribute(key) { return key in this.attrs; }
        removeAttribute(key) { delete this.attrs[key]; }
        appendChild(el) { if (el.parentNode) el.remove(); this.children.push(el); el.parentNode = this; return el; }
        insertBefore(el, before) { const index = this.children.indexOf(before); if (index < 0) return this.appendChild(el); if (el.parentNode) el.remove(); this.children.splice(index, 0, el); el.parentNode = this; return el; }
        removeChild(el) { this.children = this.children.filter(child => child !== el); el.parentNode = null; return el; }
        replaceChild(fresh, old) { this.insertBefore(fresh, old); this.removeChild(old); }
        remove() { if (this.parentNode) this.parentNode.removeChild(this); }
        replaceChildren() { this.children.slice().forEach(child => this.removeChild(child)); }
        cloneNode() { const el = new Element(this.tagName); el.attrs = {...this.attrs}; return el; }
        contains(el) { return el === this || this.children.some(child => child.contains(el)); }
        querySelectorAll(selector) { const found = [], selectors = selector.split(','); const walk = root => root.children.forEach(child => { if (selectors.some(value => matches(child, value))) found.push(child); walk(child); }); walk(this); return found; }
        querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
        focus() { if (this.isConnected) doc.activeElement = this; }
        click() { this.dispatchEvent({type: 'click'}); }
        getBoundingClientRect() { return {left: 0, top: 0, right: 120, bottom: 30, width: 120, height: 30}; }
        scrollIntoView() {}
    }
    Element.prototype.addEventListener = listen; Element.prototype.dispatchEvent = dispatch;
    Object.assign(doc, {addEventListener: listen, dispatchEvent: dispatch, createElement: tag => new Element(tag)});
    doc.body = new Element('body'); doc.body.parentNode = doc; doc.activeElement = doc.body;
    doc.querySelector = selector => doc.body.querySelector(selector); doc.getElementById = id => doc.querySelector('#' + id);
    return {doc, create: tag => new Element(tag)};
}
module.exports = makeDOM;
