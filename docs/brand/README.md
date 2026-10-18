# brigada brand

The logo is a pixel wordmark: every letter of `brigada` is built from the
same square cells. A brigada is a work crew, and each cell stands for one
agent in it. The top cell of the `b` is the foreman, the one cell drawn in
hi-vis orange.

The same grid serves the graphic logo and the terminal, so the CLI banner
and the SVG wordmark are one design.

## Files

| Path | What |
|------|------|
| `logo/wordmark-on-light.svg` | Primary logo: ink cells, for light grounds |
| `logo/wordmark-on-dark.svg` | Primary logo: paper cells, for dark grounds |
| `logo/wordmark-mono.svg` | One color (`currentColor`), no foreman accent |
| `logo/mark-*.svg` | The `b` alone, in the same three variants |
| `icon/app-icon.svg`, `icon/app-icon-light.svg` | App icon tiles |
| `icon/favicon-32.svg`, `icon/favicon-16.svg` | Favicons, drawn for their size |
| `icon/avatar.svg` | Circular avatar on hi-vis |
| `cli/banner.txt` | Terminal banner, 27 × 4 cells |
| `cli/mark.txt` | Compact terminal mark, 3 × 3 cells |
| `cli/oneline.txt` | One-line form for status lines and logs |
| `cli/ascii.txt` | ASCII fallback |
| `tokens.json` | Colors, grid and ANSI codes |

## Grid

The wordmark is 27 columns by 7 rows (`X` = cell). Rows 0–1 hold
ascenders, rows 2–4 the x-height, and row 4 sits on the baseline. Rows
5–6 hold the descender of the `g`. The cell at row 0, column 0 is the
foreman.

```text
X.......X............X.....
X....................X.....
XXX.XXX.X.XXX.XXX..XXX.XXX.
X.X.X...X.X.X.X.X..X.X.X.X.
XXX.X...X.XXX.XXXX.XXX.XXXX
............X..............
..........XXX..............
```

The mark is columns 0–2 of rows 0–4. In the SVGs a cell is 6 units, the
gap 1 unit, and the corner radius 1 unit. On the favicons the radius
drops to 0, and at 16 px the gap does too, so the mark stays sharp.

## Colors

| Token | Hex | Use |
|-------|-----|-----|
| Ink | `#111111` | Cells on light grounds; dark grounds |
| Paper | `#F5F5F2` | Cells on dark grounds; light grounds |
| Hi-vis | `#FF5A1F` | The foreman cell, and only that |
| Steel | `#5E626A` | Secondary text on paper |

On a hi-vis ground the cells are ink and the foreman cell is paper.

## Usage

- Clear space on every side: two cells.
- Minimum size: the wordmark 96 px wide, the mark 16 px tall
  (use `favicon-16.svg` there).
- Never set the wordmark in a typeface, and never pair the pixel `b` with
  typeset letters. The logo works because every letter is made of cells.
- Don't recolor cells other than the foreman, add effects, or change the
  spacing of the grid.

## Terminal

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
origin of the work. "brigada" and the pixel wordmark are trademarks of
Blackfire AI.

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
