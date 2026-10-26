package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// pageHelpers is JavaScript that runs inside the page for snapshots and element
// actions. It sees the whole page: open shadow roots and same-origin iframes
// included. Cross-origin iframes are listed as elements but not entered.
//
// A snapshot tags each visible interactive element, and each h1-h3 for context,
// with data-browse-ref (e1, e2, ...). It also remembers each ref's role and
// name, so when a framework like React replaces an element, locate can still
// find it as the one element with the same role and name.
const pageHelpers = `
const SELECTOR = [
  'a[href]', 'button', 'input:not([type=hidden])', 'select', 'textarea', 'summary', 'iframe',
  'h1', 'h2', 'h3', '[contenteditable=""]', '[contenteditable="true"]', '[onclick]',
  '[role=button]', '[role=link]', '[role=checkbox]', '[role=radio]', '[role=switch]',
  '[role=tab]', '[role=menuitem]', '[role=option]', '[role=combobox]', '[role=textbox]',
  '[role=searchbox]', '[role=slider]',
].join(',');

// walk calls visit for every element under root, entering open shadow roots
// and same-origin iframes.
function walk(root, visit) {
  for (const el of root.querySelectorAll('*')) {
    visit(el);
    if (el.shadowRoot) walk(el.shadowRoot, visit);
    if (el.tagName === 'IFRAME') {
      let doc = null;
      try { doc = el.contentDocument; } catch (e) {}
      if (doc) walk(doc, visit);
    }
  }
}

function roleOf(el) {
  const explicit = el.getAttribute('role');
  if (explicit) return explicit;
  const tag = el.tagName.toLowerCase();
  if (tag === 'input') {
    const type = (el.type || 'text').toLowerCase();
    if (['button', 'submit', 'reset', 'image'].includes(type)) return 'button';
    if (type === 'checkbox' || type === 'radio' || type === 'file') return type;
    if (type === 'range') return 'slider';
    return 'textbox';
  }
  if (el.isContentEditable) return 'textbox';
  if (/^h[1-6]$/.test(tag)) return 'heading';
  return {a: 'link', select: 'combobox', textarea: 'textbox', summary: 'button'}[tag] || tag;
}

function clean(text) {
  return (text || '').trim().replace(/\s+/g, ' ').slice(0, 80);
}

function nameOf(el) {
  const doc = el.ownerDocument;
  const by = el.getAttribute('aria-labelledby');
  if (by) {
    const text = by.split(/\s+/).map((id) => doc.getElementById(id)?.innerText || '').join(' ');
    if (clean(text)) return clean(text);
  }
  if (el.getAttribute('aria-label')) return clean(el.getAttribute('aria-label'));
  if (el.labels && el.labels.length) return clean([...el.labels].map((l) => l.innerText).join(' '));
  if (el.tagName === 'INPUT' && ['button', 'submit', 'reset'].includes(el.type)) return clean(el.value);
  if (el.tagName !== 'SELECT' && el.tagName !== 'IFRAME' && clean(el.innerText)) return clean(el.innerText);
  return clean(el.placeholder || el.getAttribute('title') || el.getAttribute('alt') ||
    el.querySelector?.('img[alt]')?.alt || (el.tagName === 'IFRAME' ? el.src : ''));
}

function visible(el) {
  const box = el.getBoundingClientRect();
  if (box.width === 0 || box.height === 0) return false;
  return el.checkVisibility ? el.checkVisibility({visibilityProperty: true}) : true;
}

// offset is where el's frame sits in the top page, so boxes inside same-origin
// iframes can be turned into top-level coordinates.
function offset(el) {
  let x = 0, y = 0, win = el.ownerDocument.defaultView;
  while (win && win.frameElement) {
    const box = win.frameElement.getBoundingClientRect();
    x += box.left + win.frameElement.clientLeft;
    y += box.top + win.frameElement.clientTop;
    win = win.parent;
  }
  return {x, y};
}

function describe(el, ref) {
  const d = {ref, role: roleOf(el), name: nameOf(el)};
  const type = (el.type || '').toLowerCase();
  if (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA') {
    if (type === 'checkbox' || type === 'radio') d.checked = el.checked;
    else if (type === 'password') d.value = el.value ? '(filled)' : '';
    else if (type !== 'file') d.value = el.value.slice(0, 200);
  }
  if (el.tagName === 'SELECT') {
    d.value = el.selectedOptions[0]?.text.trim() || '';
    d.options = [...el.options].map((o) => o.text.trim()).slice(0, 50);
  }
  const aria = el.getAttribute('aria-checked');
  if (aria) d.checked = aria === 'true';
  if (el.disabled || el.getAttribute('aria-disabled') === 'true') d.disabled = true;
  const box = el.getBoundingClientRect(), o = offset(el);
  const top = box.top + o.y, left = box.left + o.x;
  if (top > innerHeight || top + box.height < 0 || left > innerWidth || left + box.width < 0) d.offscreen = true;
  return d;
}

function snapshot(max) {
  const refs = {}, elements = [];
  let more = 0;
  walk(document, (el) => {
    el.removeAttribute('data-browse-ref');
    if (!el.matches(SELECTOR) || !visible(el)) return;
    if (elements.length === max) { more++; return; }
    const ref = 'e' + (elements.length + 1);
    el.setAttribute('data-browse-ref', ref);
    const d = describe(el, ref);
    refs[ref] = {role: d.role, name: d.name};
    elements.push(d);
  });
  window.__browseRefs = refs;
  return {url: location.href, title: document.title, elements, more};
}

function locate(ref) {
  let found = null;
  walk(document, (el) => { if (!found && el.getAttribute('data-browse-ref') === ref) found = el; });
  if (found) return found;
  const want = (window.__browseRefs || {})[ref];
  if (!want) return null;
  const matches = [];
  walk(document, (el) => {
    if (el.matches(SELECTOR) && visible(el) && roleOf(el) === want.role && nameOf(el) === want.name) matches.push(el);
  });
  if (matches.length !== 1) return null;
  matches[0].setAttribute('data-browse-ref', ref);
  return matches[0];
}

// point scrolls the element into view and returns its center in top-level
// page coordinates, where a real mouse event can be sent.
function point(ref) {
  const el = locate(ref);
  if (!el) return {missing: true};
  el.scrollIntoView({block: 'center', inline: 'center', behavior: 'instant'});
  const box = el.getBoundingClientRect(), o = offset(el);
  if (box.width === 0 || box.height === 0) return {hidden: true};
  return {x: o.x + box.left + box.width / 2, y: o.y + box.top + box.height / 2};
}

// focusField focuses an editable element and selects its contents, so typed
// text replaces them.
function focusField(ref) {
  const el = locate(ref);
  if (!el) return 'missing';
  el.scrollIntoView({block: 'center', behavior: 'instant'});
  el.focus();
  if (el.isContentEditable) {
    el.ownerDocument.getSelection().selectAllChildren(el);
    return 'ok';
  }
  if (typeof el.select === 'function') {
    el.select();
    return 'ok';
  }
  return 'not editable';
}

// choose picks a select option by value or by its visible text. It uses the
// prototype's value setter, so frameworks that track the value see a change.
function choose(ref, choice) {
  const el = locate(ref);
  if (!el) return {missing: true};
  if (el.tagName !== 'SELECT') return {error: 'is not a select; click it and pick an option instead'};
  const options = [...el.options];
  const want = choice.trim().toLowerCase();
  const opt = options.find((o) => o.value === choice) || options.find((o) => o.text.trim().toLowerCase() === want);
  if (!opt) return {error: 'has no option ' + JSON.stringify(choice) + '; options: ' + options.map((o) => o.text.trim()).join(', ')};
  const proto = el.ownerDocument.defaultView.HTMLSelectElement.prototype;
  Object.getOwnPropertyDescriptor(proto, 'value').set.call(el, opt.value);
  el.dispatchEvent(new Event('input', {bubbles: true}));
  el.dispatchEvent(new Event('change', {bubbles: true}));
  return {selected: opt.text.trim()};
}

function textOf(ref, max) {
  const el = ref ? locate(ref) : document.body;
  if (!el) return {missing: true};
  const text = el.innerText || '';
  return {text: max > 0 ? text.slice(0, max) : text, total: text.length};
}

function scrollEdge(where) {
  const root = document.scrollingElement || document.documentElement;
  scrollTo(0, where === 'top' ? 0 : root.scrollHeight);
}
`

// settleJS resolves once the page has loaded and its DOM has had 300ms
// without changes, or after 3 seconds of constant change.
const settleJS = `new Promise((resolve) => {
  const quiet = () => {
    let timer;
    const done = () => { observer.disconnect(); clearTimeout(cap); resolve(true); };
    const observer = new MutationObserver(() => { clearTimeout(timer); timer = setTimeout(done, 300); });
    observer.observe(document, {subtree: true, childList: true, attributes: true, characterData: true});
    timer = setTimeout(done, 300);
    const cap = setTimeout(done, 3000);
  };
  if (document.readyState === 'complete') quiet();
  else addEventListener('load', quiet, {once: true});
})`

// pageCall returns JavaScript that calls one pageHelpers function with args.
func pageCall(fn string, args ...any) string {
	parts := make([]string, len(args))
	for i, arg := range args {
		b, _ := json.Marshal(arg) // args are strings and ints, which always marshal
		parts[i] = string(b)
	}
	return fmt.Sprintf("(() => {\n%s\nreturn %s(%s);\n})()", pageHelpers, fn, strings.Join(parts, ", "))
}
