// Shared helpers for Lantern pages.
const Lantern = {
  async api(path, body) {
    const opts = body === undefined ? {} : {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body)
    };
    const r = await fetch(path, opts);
    return r.json();
  },
  bytes(n) {
    if (!n) return '—';
    const u = ['B', 'KB', 'MB', 'GB', 'TB'];
    let i = 0;
    while (n >= 1000 && i < u.length - 1) { n /= 1000; i++; }
    return (i === 0 ? n : n.toFixed(n < 10 ? 1 : 0)) + ' ' + u[i];
  },
  duration(s) {
    if (!isFinite(s) || s <= 0) return '';
    if (s < 90) return Math.round(s) + ' sec';
    if (s < 5400) return Math.round(s / 60) + ' min';
    const h = Math.floor(s / 3600), m = Math.round((s % 3600) / 60);
    return h + ' hr ' + m + ' min';
  },
  esc(s) {
    return String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  },
  // Sorts installed ZIM files into Lantern's sections.
  classify(z) {
    const n = z.name.toLowerCase();
    if (/^wikipedia_[a-z]+_all/.test(n)) return 'wikipedia';
    if (n.startsWith('gutenberg')) return 'books';
    return 'library';
  },
  // Friendly names for Kiwix's Gutenberg subject collections (Library of Congress classes).
  gutenbergSections: {
    'pz': "Fiction & children's books", 'ps': 'American literature', 'pr': 'English literature',
    'pn': 'Literature (general)', 'pq': 'French, Italian, Spanish & Portuguese literature',
    'pt': 'German & Scandinavian literature', 'pa': 'Greek & Latin classics', 'pe': 'English language',
    'q': 'Science', 't': 'Technology & engineering', 's': 'Agriculture, farming & animals', 'r': 'Medicine',
    'u': 'Military science', 'v': 'Naval science', 'g': 'Geography, anthropology & recreation',
    'h': 'Social sciences & economics', 'j': 'Political science', 'k': 'Law', 'l': 'Education',
    'b': 'Philosophy, psychology & religion', 'c': 'History: auxiliary sciences', 'd': 'World history',
    'e': 'History of the Americas (part 1)', 'f': 'History of the Americas (part 2)',
    'm': 'Music', 'n': 'Fine arts', 'a': 'General works & encyclopedias', 'z': 'Bibliography & libraries',
  },
  gutenbergLabel(base) {
    if (base === 'gutenberg_en_all') return 'Entire library — all subjects';
    const m = base.match(/^gutenberg_en_lcc-([a-z]+)$/);
    return m ? (this.gutenbergSections[m[1]] || m[1].toUpperCase()) : base;
  },
  prettyName(name) {
    const known = {
      wikipedia_en_all_maxi: 'Wikipedia (full, with images)',
      wikipedia_en_all_nopic: 'Wikipedia (text only)',
      wikipedia_en_all_mini: 'Wikipedia (summaries)',
      wikipedia_en_medicine_maxi: 'WikiMed medical encyclopedia',
      gutenberg_en_all: 'Project Gutenberg library',
      ifixit_en_all: 'iFixit repair guides',
      wikibooks_en_all_maxi: 'Wikibooks',
      wikivoyage_en_all_maxi: 'Wikivoyage travel guides',
    };
    const base = name.replace(/_\d{4}-\d{2}$/, '');
    const date = (name.match(/_(\d{4}-\d{2})$/) || [])[1];
    if (/^gutenberg_en_lcc-/.test(base)) {
      return 'Gutenberg: ' + this.gutenbergLabel(base) + (date ? ' · ' + date : '');
    }
    const label = known[base] || base.replace(/[_-]+/g, ' ').replace(/\b\w/g, c => c.toUpperCase());
    return date ? label + ' · ' + date : label;
  },
  bookUrl(name) { return '/kiwix/viewer#' + encodeURIComponent(name); },
  markNav(id) {
    document.querySelectorAll('.nav a').forEach(a => a.classList.toggle('on', a.dataset.id === id));
  },
};
