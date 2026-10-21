# brigada brand

brigada uses the Blackfire AI mark: the Blackfire flame, an ink body with a
hi-vis core. Blackfire AI products don't get symbols of their own. They
differ by name and descriptor only, so every product lockup is the flame,
the product name and `BY BLACKFIRE AI`.

## Files

| Path | What |
|------|------|
| `logo/lockup-on-light.svg` | Primary logo: flame + **brigada** / BY BLACKFIRE AI, for light grounds |
| `logo/lockup-on-dark.svg` | The same for dark grounds (paper flame and name) |
| `logo/lockup-mono.svg` | One color (`currentColor`), the core cut out of the flame |
| `logo/mark-*.svg` | The flame alone, in the same three variants |
| `icon/app-icon.svg`, `icon/app-icon-light.svg` | App icon tiles: ink tile with a paper flame; white tile with an ink flame |
| `icon/favicon-32.svg`, `icon/favicon-16.svg` | Favicons (the ink tile) |
| `icon/avatar.svg` | Circular avatar: ink flame, paper core on hi-vis |
| `cli/*.txt` | Terminal forms (see [Terminal](#terminal)) |
| `tokens.json` | Colors, type and ANSI codes |

The lockups have their text converted to paths, so they render without the
font installed.

## Lockup

- **Name:** Space Grotesk 700, lower case, tracking −0.035 em.
- **Descriptor:** `BY BLACKFIRE AI` in Space Grotesk 500, caps, tracking
  +0.42 em, at one fifth of the name size, in Steel.
- **Flame:** as tall as the name plus descriptor, to the left of the name,
  with a gap of about a quarter of its height.

The Blackfire AI lockup is built the same way, with `blackfire` and
`AI INFRASTRUCTURE`.

## Colors

| Token | Hex | Use |
|-------|-----|-----|
| Ink | `#111111` | Flame and name on light grounds; dark grounds |
| Paper | `#F5F5F2` | Flame and name on dark grounds; light grounds |
| Hi-vis | `#FF5A1F` | The flame core, and only that |
| Steel | `#555555` | The descriptor on light grounds (`#A3A7AE` on dark) |

On a hi-vis ground the flame is ink and the core is paper.

## Usage

- Clear space on every side: the height of the flame core.
- Minimum size: the lockup 120 px wide, the flame 16 px tall (use the
  favicon tile below 24 px).
- Don't set the lockup in another typeface, recolor the flame or its core,
  add effects, or draw a separate brigada symbol.

## Terminal

The terminal keeps the earlier pixel wordmark: a flame doesn't draw well in
character cells, and `internal/logo` prints these files byte for byte. The
one hi-vis cell stands for the flame core.

```text
█       ▀            █
█▀█ █▀▀ █ █▀█ █▀█  █▀█ █▀█
▀▀▀ ▀   ▀ ▀▀█ ▀▀▀▀ ▀▀▀ ▀▀▀▀
          ▀▀▀
```

Each character holds two cells, one above the other (half blocks), so
the cells stay square in a terminal.

- **Foreman cell:** print the first character of the banner and the mark
  as `▄` on an orange background. Its lower half takes the terminal's own
  foreground, so it reads on light and dark themes.
  `ESC[48;2;255;90;31m▄ESC[0m` (truecolor), `ESC[48;5;202m` (256 colors),
  `ESC[41m` (16 colors).
- **One-line form:** `▙ brigada`, with `▙` in `ESC[38;2;255;90;31m`,
  `ESC[38;5;202m` or `ESC[31m`.
- **Every other cell:** the terminal's default foreground, with no color
  code.
- **`NO_COLOR` set or output not a terminal:** no escape codes; the
  foreman cell prints as `█`.
- **Locale not UTF-8, or `TERM=dumb`:** print `cli/ascii.txt`.
- **Narrower than 28 columns:** use the one-line form only.

## License and trademark

The files in this directory are covered by the repository's
[Apache License 2.0](../../LICENSE), like the code.

The license covers the files, not the name. As section 6 of the license
says, it grants no right to use the brigada name or logo as a trademark,
except as required for reasonable and customary use in describing the
origin of the work. "Blackfire AI", "brigada" and the flame mark are
trademarks of Blackfire AI.

The rules below are trademark guidance. They don't change what the
Apache license lets you do with the files themselves.

You may, without asking:

- use the name and the unmodified logo to refer to brigada, for example
  in articles, talks, documentation, and in integrations that work with
  it;
- say that your project works with, or is built on, brigada.

Please don't:

- use the name or logo for another product or service, or as part of
  its name or logo;
- modify the logo, or combine it with another mark;
- suggest that Blackfire AI endorses or sponsors your work without its
  written permission.

If a use falls outside these rules, open an issue and ask.
