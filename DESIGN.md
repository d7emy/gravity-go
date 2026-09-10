# binq.cc — Frontend Design Specification

> Complete design reference extracted from `binq/web/templates/index.html`, `binq/web/static/css/app.css`, and `binq/web/static/js/app.js`.

---

## Technology Stack

| Layer      | Choice                                                                 |
| :--------- | :--------------------------------------------------------------------- |
| Font       | [Comfortaa 700](https://fonts.googleapis.com/css?family=Comfortaa:700) |
| CSS        | Vanilla CSS with CSS custom properties (no preprocessor)               |
| Grid/Utils | Bootstrap 5.3.8 (CSS + JS bundle)                                     |
| Icons      | Bootstrap Icons 1.11.3                                                 |
| JS         | Vanilla JS (single IIFE in `app.js`, no framework)                    |
| Routing    | Client-side SPA via `history.pushState`, 4 pages                       |
| Templating | Go `html/template` — single `index.html` serves all pages             |

---

## Color System

### Light Theme (default)

| Token                  | Value                    | Usage                       |
| :--------------------- | :----------------------- | :-------------------------- |
| `--color-primary`      | `#074d31`                | Headings, accents, buttons  |
| `--color-primary-light` | `#1b8354`               | Gradients, hover states     |
| `--color-primary-rgb`  | `7, 77, 49`              | For `rgba()` usage          |
| `--color-accent`       | `#d3b98c`                | Decorative underlines       |
| `--color-accent-rgb`   | `211, 185, 140`          | For `rgba()` usage          |
| `--color-danger`       | `#ff4d4f`                | Error states                |
| `--color-success`      | `#52c41a`                | Success states              |
| `--color-bg`           | `#f8f9fa`                | Page background             |
| `--color-surface`      | `#ffffff`                | Cards, inputs, tables       |
| `--color-text`         | `#1f2937`                | Primary text                |
| `--color-text-muted`   | `rgba(0, 0, 0, 0.45)`   | Secondary text              |
| `--color-text-dim`     | `rgba(0, 0, 0, 0.35)`   | Tertiary text               |
| `--color-text-faint`   | `rgba(0, 0, 0, 0.30)`   | Placeholders, empty states  |
| `--color-text-hint`    | `rgba(0, 0, 0, 0.25)`   | Lightest text               |
| `--color-star`         | `#f5b301`                | Favorite star icon          |
| `--color-border`       | `rgba(0, 0, 0, 0.08)`   | Card/section borders        |
| `--color-border-btn`   | `rgba(0, 0, 0, 0.15)`   | Button borders              |
| `--color-spinner-track` | `rgba(0, 0, 0, 0.1)`   | Spinner background ring     |

### Dark Theme (`[data-theme="dark"]`)

| Token                  | Value                          |
| :--------------------- | :----------------------------- |
| `--color-primary`      | `#2fad73`                      |
| `--color-primary-light` | `#45c988`                     |
| `--color-primary-rgb`  | `47, 173, 115`                 |
| `--color-accent`       | `#c49b60`                      |
| `--color-accent-rgb`   | `196, 155, 96`                 |
| `--color-bg`           | `#121e17`                      |
| `--color-surface`      | `#1c2c23`                      |
| `--color-text`         | `#dce5e0`                      |
| `--color-text-muted`   | `rgba(220, 229, 224, 0.5)`     |
| `--color-text-dim`     | `rgba(220, 229, 224, 0.42)`    |
| `--color-text-faint`   | `rgba(220, 229, 224, 0.35)`    |
| `--color-text-hint`    | `rgba(220, 229, 224, 0.28)`    |
| `--color-star`         | `#f5c842`                      |
| `--color-border`       | `rgba(255, 255, 255, 0.1)`     |
| `--color-border-btn`   | `rgba(255, 255, 255, 0.15)`    |
| `--color-spinner-track` | `rgba(255, 255, 255, 0.1)`    |

### Dark Mode Overrides (non-variable)

These selectors override hardcoded colors that can't use variables:

```css
[data-theme="dark"] .navbar-main          /* box-shadow: darker */
[data-theme="dark"] .bin-textarea          /* border-color: var(--color-border) */
[data-theme="dark"] .bin-table thead th    /* background: rgba(255,255,255,0.03) */
[data-theme="dark"] .star-btn             /* color: rgba(220,229,224,0.28) */
[data-theme="dark"] .bin-card-row          /* color: rgba(220,229,224,0.6) */
[data-theme="dark"] .page-btn             /* border: var(--color-border) */
[data-theme="dark"] .code-block           /* background/border/color overridden */
[data-theme="dark"] .filter-select        /* dark chevron SVG */
[data-theme="dark"] .upload-msg.error     /* #ff6b6b instead of #cf1322 */
```

### Theme Persistence

- Stored in cookie `binq_theme` (values: `"light"` or `"dark"`)
- Applied before render via an inline `<script>` block in `<head>` that reads the cookie and sets `data-theme` on `<html>`
- Toggle button `#themeToggle` switches theme and updates cookie

---

## Design Tokens

### Shadows

| Token                | Value                                                                |
| :------------------- | :------------------------------------------------------------------- |
| `--shadow-card`      | `0 2px 8px rgba(0,0,0,0.06)` · dark: `rgba(0,0,0,0.3)`             |
| `--shadow-elevated`  | `0 8px 24px rgba(0,0,0,0.12), 0 4px 12px rgba(0,0,0,0.08)` · dark: `0.4/0.3` |

### Border Radii

| Token           | Value  | Usage                   |
| :-------------- | :----- | :---------------------- |
| `--radius-sm`   | `4px`  | Dropdowns, copy buttons |
| `--radius-md`   | `8px`  | Inputs, toasts, code    |
| `--radius-lg`   | `14px` | Cards, textarea, table  |
| `--radius-xl`   | `20px` | *(reserved)*            |
| `--radius-pill` | `50px` | Buttons, nav links      |

### Transitions

| Token                  | Value        |
| :--------------------- | :----------- |
| `--transition-base`    | `0.3s ease`  |
| `--transition-smooth`  | `0.2s ease`  |

---

## Animations

| Name         | Usage                          | Definition                                         |
| :----------- | :----------------------------- | :------------------------------------------------- |
| `Gradient`   | `#title` background shimmer   | Shifts `background-position` 0→100→0% over 20s     |
| `rotate`     | `.spinner` loading indicator   | 360° rotation in 0.6s linear, infinite              |
| `fadeIn`     | `.fade-in` result rows/cards   | Opacity 0→1 + translateY(4px→0) over 0.35s          |

### `prefers-reduced-motion: reduce`

- `#title` animation: none
- `.section::before` transition: none
- `.spinner` animation: none

---

## Typography

| Element            | Font                    | Size   | Weight | Notes                       |
| :----------------- | :---------------------- | :----- | :----- | :-------------------------- |
| Body               | Comfortaa, cursive      | —      | bold   | Global default              |
| `#title`           | inherit                 | 36px   | bold   | Gradient text fill          |
| `.subtitle`        | inherit                 | 15px   | normal |                             |
| `.subtitle-soft`   | inherit                 | 12px   | normal | Dimmer color                |
| `.nav-link-custom` | inherit                 | 0.85rem | —     |                             |
| `.navbar-brand`    | inherit                 | 1.15rem | 800   |                             |
| `.bin-table`       | inherit                 | 13px   | normal |                             |
| `.bin-table th`    | inherit                 | 11px   | bold   | Uppercase, letter-spacing 1px |
| `.td-bin`          | Courier New, monospace  | —      | bold   | Primary color               |
| `.code-block`      | Courier New, monospace  | 12px   | normal |                             |
| `.btn_one/two`     | Comfortaa, cursive      | 14px   | bold   |                             |
| `.btn_small`       | Comfortaa, cursive      | 10px   | bold   |                             |
| `.user-name`       | inherit                 | 0.85rem | 800   | Max 100px, ellipsis         |
| `#footer`          | inherit                 | 11px   | —      |                             |

---

## Layout Structure

```
┌─────────────────────────────────────────────┐
│ .top-bar  (4px gradient strip, fixed top)   │
├─────────────────────────────────────────────┤
│ .navbar-main  (sticky top:4px, z:1020)      │
│   logo  |  lookup · browse · starred · api  │
│          theme-toggle · auth area           │
├─────────────────────────────────────────────┤
│ .app  (centered column, flex:1)             │
│   ┌─ #pageLookup ───────────────────┐       │
│   │  title + subtitle               │       │
│   │  textarea + lookup button       │       │
│   │  filter bar (include/exclude)   │       │
│   │  active filter chips            │       │
│   │  results table / cards          │       │
│   │  pagination                     │       │
│   └──────────────────────────────────┘       │
│   ┌─ #pageBrowse  (display:none) ───┐       │
│   ┌─ #pageFavorites (display:none) ─┐       │
│   ┌─ #pageDocs    (display:none) ───┐       │
├─────────────────────────────────────────────┤
│ #footer                                     │
└─────────────────────────────────────────────┘
```

### `.app` Container Breakpoints

| Breakpoint      | `max-width` | Padding                |
| :-------------- | :---------- | :--------------------- |
| ≤ 400px         | 100%        | `12px 8px 40px`        |
| ≤ 640px         | 100%        | `16px 10px 40px`       |
| 641px – 1023px  | `640px`     | `28px 20px 48px`       |
| 1024px – 1399px | `960px`     | `36px 24px 56px`       |
| ≥ 1400px        | `1100px`    | `44px 32px 64px`       |

---

## Navigation

### Top Bar

- 4px-tall gradient: `primary → primary-light → accent`
- Fixed position, z-index 1030

### Navbar

- Sticky at `top: 4px`, z-index 1020
- Background: `--color-surface`, subtle box-shadow
- Left: brand logo (`bi-credit-card-2-front` + "binq.cc")
- Right: nav links (lookup, browse, starred, api) + theme toggle + auth area
- Active link gets `.active` class: bold + primary color
- Hover: primary color + subtle background

### Mobile Navbar (≤ 640px)

- Brand text hidden (font-size: 0), only icon visible
- Nav link text hidden, only icons visible (font-size: 0)
- Active link gets colored background
- Tighter padding/gaps

---

## Pages

### 1. Lookup (`/`)

**Hero section:**
- `#title` — gradient animated text: "binq.cc"
- `.subtitle` — "fast BIN lookup — check card issuer, brand, type & country"
- `.subtitle-soft` — "6-digit or 8-digit BINs · bulk lookup supported"
- Gold accent underline (50px bar under title)

**Input:**
- `<textarea>` with multi-line placeholder, max-width 680px
- Pill-shaped lookup button (`.btn_two`, filled primary)
- Status/error message area

**Results toolbar:**
- Count display + Export CSV/JSON + Clear filters buttons

**Filter bar (include):**
- Brand dropdown (single-select custom)
- Type dropdown (single-select custom)
- Category dropdown (multi-select checkbox)
- Country dropdown (multi-select checkbox)
- Issuer text input

**Exclude filter bar:**
- Toggled via `.exclude-toggle` button (chevron rotates 180°)
- Same structure as include filters but with "exclude" labels
- Hidden by default (`.exclude-hidden`)

**Active filter chips:**
- Include chips: green-tinted background + primary text
- Exclude chips: red-tinted background (`.exclude-chip`)
- Each chip has a "×" remove button

**Results table:**
- Columns: BIN · Brand · Type · Category · Issuer/Bank · Country · Actions
- Sortable columns with sort indicators
- BIN column: monospace, primary color
- Row hover: faint primary tint
- Star button for favorites
- Copy button for BIN
- Not-found rows: italic, faint color, dashed border (mobile card)

**Mobile cards (≤ 768px):**
- Table hidden, replaced with `.bin-card` stack
- Card header: BIN (monospace) + flag
- Key-value rows

**Pagination:**
- Centered pill buttons
- Active page: filled primary
- Page info text

### 2. Browse (`/browse`)

- Title: "browse."
- Subtitle: "explore the BIN database"
- Filter bar: BIN prefix input, Brand/Type/Category selects, Country/Issuer inputs, Search button
- Same results table/cards/pagination as Lookup
- Server-side pagination via API (`limit`/`offset`)

### 3. Starred (`/favorites`)

- Title: "starred."
- Subtitle: "your saved BINs" + "stored privately for this browser"
- Toolbar with count + export buttons
- Same table/cards layout
- Empty state: "no starred BINs yet — click the ☆ on any result to save it here"

### 4. API Docs (`/docs`)

- Title: "api."
- `.section` cards with animated top-border reveal on hover
- Each section: endpoint title (uppercase, letter-spaced) + description + code block
- Endpoints documented:
  - `GET /api/bin/:bin`
  - `POST /api/bin/bulk`
  - `GET /api/bin/search`
  - `GET /api/stats`
  - `GET /healthz`

---

## Component Catalog

### Buttons

| Class       | Style                                             | Size       |
| :---------- | :------------------------------------------------ | :--------- |
| `.btn_one`  | Outlined primary, fills on hover                  | 10px 24px  |
| `.btn_two`  | Filled primary, becomes outlined on hover         | 10px 24px  |
| `.btn_small` | Outlined muted, primary on hover                 | 5px 12px   |
| `.page-btn` | Outlined, primary on hover, filled when `.active` | 6px 12px   |
| `.copy-btn` | Ghost, icon only, primary on hover                | 2px 6px    |
| `.star-btn` | Ghost, icon only, gold when `.starred`            | 2px 6px    |

All buttons use `--radius-pill` (50px).

### Inputs

| Element           | Style                                                 |
| :---------------- | :---------------------------------------------------- |
| `.bin-textarea`   | 2px border, `--radius-lg`, surface bg, shadow-card    |
| `.filter-input`   | 1px border, `--radius-md`, surface bg                 |
| `.filter-select`  | Same + custom SVG chevron, `appearance: none`         |

Focus state: primary border + 3px ring `rgba(primary, 0.08)`.

### Custom Dropdowns

**Single-select (`.single-select-dropdown`):**
- Toggle button with label + chevron
- Dropdown menu: absolute positioned, z-1000, surface bg, shadow
- Items highlight on hover, `.selected` gets bolder + tinted bg
- Chevron rotates 180° when `.open`

**Multi-select checkbox (`.checkbox-dropdown`):**
- Same toggle/menu structure
- Search input at top
- Scrollable items list (max-height 200px)
- Each item: checkbox + label
- Accent-colored checkboxes

### Table (`.bin-table`)

- Full-width, collapse borders
- Surface background, rounded corners (`--radius-lg`)
- Card shadow + 1px border
- Header: uppercase, letter-spaced, muted color, faint bg
- Sortable headers: cursor pointer, hover primary, `.sort-active` primary color
- Cells: 10px 12px padding
- Row hover: faint primary tint
- Last row: no bottom border

### Cards (`.bin-card`, mobile only)

- Surface background, 1px border, `--radius-lg`, card shadow
- Header: BIN (monospace, primary) + flag image
- Key-value rows: flex between, 12px font

### Toast

- Fixed, centered top (60px)
- Surface bg, border, elevated shadow
- Slide-in animation: translateY(-20px→0) + opacity
- Auto-dismiss after 2400ms
- Max-width 90%

### Section (API docs, `.section`)

- Surface bg, 1px border, `--radius-lg`, card shadow
- Animated top border: 3px gradient primary→primary-light, `scaleX(0→1)` on hover
- `.section-title`: uppercase, letter-spaced, border-bottom with 40px accent underline
- `.code-block`: faint bg, monospace, `pre-wrap`

### Spinner

- 16×16px, 2px border ring
- Track: `--color-spinner-track`, active: `--color-primary`
- 0.6s infinite linear rotation

### Empty State

- Centered text, faint color, 40px vertical padding

---

## Scrollbar

```css
*::-webkit-scrollbar        { width: 4px; }
*::-webkit-scrollbar-track  { background: transparent; }
*::-webkit-scrollbar-thumb  { background: rgba(0,0,0,0.15); border-radius: 3px; }
/* dark */ rgba(255,255,255,0.2)
```

---

## Responsive Breakpoints Summary

| Breakpoint        | Key Changes                                                       |
| :---------------- | :---------------------------------------------------------------- |
| `pointer: coarse` | Larger tap targets: star 18px, copy 16px, page-btn 40px min-h    |
| ≤ 400px           | Smallest layout, reduced font/padding                             |
| ≤ 640px           | Icon-only navbar, full-width inputs, vertical lookup actions      |
| ≤ 768px           | Table hidden → card view, vertical filter bar                     |
| ≥ 641px           | `max-width: 640px` container, larger title/buttons/textarea       |
| ≥ 1024px          | `max-width: 960px` container, title 48px                          |
| ≥ 1400px          | `max-width: 1100px` container                                     |

---

## Accessibility

- `aria-live="polite"` on `#srStatus` (screen reader announcements)
- `aria-live="polite"` + `role="status"` on toast
- `role="alert"` on lookup message area
- `aria-label` on theme toggle button
- `:focus-visible` outlines: 2px primary, 2px offset
- `.sr-only` utility class for visually hidden content
- `prefers-reduced-motion: reduce` — disables all animations

---

## Favicon

Inline SVG data URI: 💳 emoji at 28px in a 32×32 viewBox.

---

## Static Assets

### Brand Logo SVGs (`/static/img/brands/`)

| File             | Brand      |
| :--------------- | :--------- |
| `visa.svg`       | Visa       |
| `mastercard.svg` | Mastercard |
| `amex.svg`       | Amex       |
| `discover.svg`   | Discover   |
| `jcb.svg`        | JCB        |
| `maestro.svg`    | Maestro    |
| `diners.svg`     | Diners     |
| `unionpay.svg`   | UnionPay   |
| `mir.svg`        | Mir        |
| `rupay.svg`      | RuPay      |
| `generic.svg`    | Fallback   |

Usage: `.brand-logo` (28×18px) and `.brand-logo-lg` (36×22px) classes.

---

## JavaScript App Architecture

### State Object

```js
{
  page,                 // "lookup" | "browse" | "docs" | "favorites"
  results,              // lookup raw results
  filteredResults,      // after client-side filtering
  lookupPage,           // pagination offset
  lookupPerPage: 100,
  browseResults,        // browse page data
  browseTotal,          // total count from API
  browseOffset,
  browseLimit: 50,
  filters,              // include filter state
  excludeFilters,       // exclude filter state
  browseFilters,        // browse page filter state
  brandList, typeList, categoryList, countryList,  // filter options
  selectedCategories, selectedCountries,            // multi-select include
  excludeCategories, excludeCountries,              // multi-select exclude
  favorites,            // { bin: record } map
  favResults,           // array of favorite records
  lookupSort,           // { col, dir }
  browseSortState       // { col, dir }
}
```

### Client-Side Routing

- SPA routing via `history.pushState` / `popstate`
- `data-nav` attributes on links trigger `navigate(path)`
- Route map: `/` → lookup, `/browse` → browse, `/docs` → docs, `/favorites` → favorites
- Pages shown/hidden via `display: none`

### Key Features

- **Bulk lookup**: textarea accepts comma/space/newline-separated BINs
- **Full card auto-detection**: long numbers trimmed to BIN prefix
- **Client-side filtering**: brand, type, category, country, issuer (include + exclude)
- **Client-side sorting**: click column headers to cycle asc/desc
- **Server-side browse**: paginated search via `/api/bin/search`
- **Favorites**: stored in `state.favorites`, persisted per-browser
- **Export**: CSV and JSON download for any result set
- **Copy to clipboard**: individual BIN copy button
- **Toast notifications**: for copy, export, errors
- **Auth**: Telegram login widget + session cookie (`binq_session`)
- **Theme**: toggle between light/dark, persisted via cookie (`binq_theme`)

---

## SEO / Meta

```html
<title>binq.cc — BIN Lookup</title>
<meta name="description" content="Fast, clean BIN (Bank Identification Number) lookup. Check card issuer, brand, type, and country.">
<meta property="og:title" content="binq.cc — BIN Lookup">
<meta property="og:description" content="Fast, clean BIN lookup tool. Check card issuer, brand, type, and country.">
```

---

## External Dependencies

| Dependency          | Version | Source                       |
| :------------------ | :------ | :--------------------------- |
| Bootstrap CSS       | 5.3.8   | jsdelivr CDN                 |
| Bootstrap Icons     | 1.11.3  | jsdelivr CDN                 |
| Bootstrap JS Bundle | 5.3.8   | jsdelivr CDN                 |
| Comfortaa Font      | 700     | Google Fonts CDN             |
