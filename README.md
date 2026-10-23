# SFTPGo - DPM Edition

A fork of [drakkan/sftpgo](https://github.com/drakkan/sftpgo) that runs the DPM Underground LIVE file server (`files.drpolishmatt.com`). It tracks upstream `main` and adds the share and delivery features listed below. The upstream README follows after the fork notes.

## DPM Edition modifications

### Shares

- **Default share name.** A share created for a single file is named after the file. A share created for a single folder gets a name from a per-user template (admin > Users > Profile > "Default folder share name"), where `{parent}` is the parent folder name and `{name}` the folder name. The default template is `{parent} - {name}`, so `Band/Mix` becomes `Band - Mix`.
- **Default share max tokens.** A per-user setting (admin > Users > Profile) for the use limit of new shares. `0` means unlimited.
- **Default expiration.** New shares use SFTPGo's built-in per-user "Default shares expiration".
- **Shares list.** A Created column, sorted newest first; a file or folder icon per share replaced by its preview when there is one, a missing badge for paths that are gone, folder names linking to the folder; the links dialog offers only the links that fit (zip and browse for folders, the file page for files). Deleting a share says clearly that only the share link is removed and the shared files stay.
- **Folder shares: Download all (.zip).** A button on the folder share page and the zip link in a read-only field with a Copy button.
- **Resumable zips without a zip file.** Every web zip download (the share zip link, a selection in a folder share or in the web client, the REST API) is assembled on the fly from the original files and never written anywhere (`internal/httpd/resumablezip.go`):
  - files are stored without compression (media does not shrink, the zips are for keeping files organized), so it streams as fast as the disks deliver;
  - the layout is fixed (zip64 for every entry, names sorted per folder, folders dated by their newest file, `.cache` left out), so the size is known before the first byte: downloads show a real size and time remaining;
  - `Range` and `If-Range` are honored with an ETag built from every name, size and modification time: an interrupted download resumes at any byte with identical bytes, and a folder that changed in between gets a fresh full download instead;
  - CRC-32 checksums are computed while files stream and saved with a checkpoint every 64 MB in the folder's `.cache` (`<file name>.zipcrc.json`), so a resume rereads at most 64 MB; without saved checksums the missing ones are computed by reading the files;
  - resumes and HEAD requests do not count as share uses.

  Tested with Info-ZIP unzip, Python, 7-Zip and libarchive; zip64 tested with a 4.5 GiB file.
- **Dead shares are deleted.** An hourly task deletes shares that expired, used up their download limit or point to files that no longer exist (unless many look missing at once, which means unreachable storage), and clears the previews and zip checksums kept for them in `.cache` (unless another live share covers the same files). Until then the link says why it stopped working and the owner can still extend the share; afterwards it says the link is no longer available (it may have expired, reached its download limit or been removed). Deleting a share by hand clears its cache too (`internal/httpd/sharecleanup.go`).
- **Hidden entries stay out of public shares.** Names starting with a dot (`.cache`, `.tmp`, `.DS_Store`, macOS `._` files) and everything inside dot folders are not listed, not added to zip downloads and cannot be downloaded through a public share, even when requested by path. The owner still sees them over SFTP, WebDAV and the logged-in web client. This is done by merging a hide rule for `.*` into the owner's pattern rules on every share connection (`internal/httpd/sharehidden.go`).

### Media previews for single-file shares

The share page (`/web/client/pubshares/<id>/download`) of a single video, audio or image file shows a player and the file details (`internal/httpd/sharepreview.go`, `templates/webclient/sharedownload.html`).

- **Video:** the browser's native player streams the original file, with range requests for seeking and no autoplay. The poster comes in passes: the share card at once, the middle frame within seconds, then a background pick replaces it (9 moments, 3 tiny frames each, black frames and cuts dropped, the rest scored on stillness, contrast, colorfulness and exposure). Details: container, duration, dimensions, frame rate, codec, bit rate, camera and lens (from the Sony XML sidecar, the DJI encoder tag or QuickTime make and model tags), the source (camera original, DaVinci Resolve render, ffmpeg output) and size.
- **Audio:** a waveform you can click to seek, drawn from per-bucket loudness (RMS), over the browser's audio player. The player streams a 128 kbps MP3 preview (stereo when the source is stereo); Download gets the original. Details: format, duration, channels, sample rate, bit depth (lossless sources), size.
- **Image:** a scaled preview up to 1600 px; Download gets the full-resolution original. Camera raw files (DNG, ARW, CR2, CR3, NEF, RAF, ORF, RW2) are shown from the JPEG rendering embedded in the file, rotated by its orientation tag. Details: dimensions, camera (in capitals) and lens when the file carries them, size.
- **Layout:** the file name and a Download button with the file size in the header, the preview on a dark stage, details as chips, and the direct download link in a read-only field with a Copy button.
- **Link previews in chat apps:** the page carries Open Graph and Twitter tags (title, details, picture and its size), so Facebook, Discord, Tumblr, Slack and others unfold a pasted link into a card; clicking the card opens the share page. Link cards are always of the video type for audio and video. H.264 videos up to 100 MB are offered as they are; other videos get a small H.264 copy (`.card.mp4`, long side 960 px, fastest encoder preset, always under 100 MB: the bitrate is planned for 90 MB and the output is cut at 95 MB), rendered after the poster when the share is created. Audio shares also get a small card video (`.card.mp4` in `.cache`: the branded waveform as a still picture over 128 kbps AAC, about 1.2 MB per minute), rendered when the share is created (or on the first look at its page) and advertised as `og:video` once ready, so Discord can play the audio in the card. Like the page player, the played part of the waveform turns from gray to color as it plays, with the file name on top and the details at the bottom in Dogica Pixel (SIL Open Font License; built into the binary from `internal/httpd/fonts/`, and shipped with its license in the image at `/usr/share/sftpgo/fonts/`). It is rendered again when the branding logo changes. A direct download link gets the same card for link preview bots while people get the file; zip shares get a card too. Previews are prepared when a share is created or edited (and once after a restart for live shares); a picture not ready within 2 seconds is answered with a branded share card and the page versions the picture address so later looks find the real one (`internal/httpd/sharecards.go`).
- **robots.txt:** search engines and scrapers are kept out; only link preview bots may read share pages and their pictures.
- **Branding:** the audio waveform card and the waveform on the page show the current branding logo in the bottom-right corner, applied at request time, so a logo change in the admin branding settings takes effect at once.
- **Generated files** live in a `.cache` folder next to the shared file (`<file name>.probe.json`, `.thumb.jpg`, `.peaks.json`, `.preview.mp3`). They are created with ffmpeg on first view, regenerated when missing or when the source file changes, and never block the download.
- **Thumbnails in file lists:** images (camera raw files included) and videos get a 40 px thumbnail in the logged-in file list and in folder shares. The file icon shows until the thumbnail has loaded and stays when none can be made. Thumbnails are 256 px square center crops (`.thumb-sm.jpg`), loaded lazily and cached by browsers until the file changes. Audio keeps its icon, since a waveform needs a full decode.
- **Cache cleanup:** folders listed in the web client or through a share are queued (at most every 10 minutes per folder) for a background check that removes previews whose original was deleted, renamed or changed, leftovers of interrupted jobs, and the `.cache` folder once empty. Only file names written by the preview code are touched (`internal/httpd/sharepreviewsweep.go`).
- **Share uses:** preview requests honor expiry, passwords and permissions but do not count as share uses, since a player sends many range requests while seeking.
- Enabled with `SHARE_PREVIEWS=true`. ffmpeg and exiftool are installed in the Docker image.

### Background tasks

One task manager (`internal/httpd/tasks.go`) runs the work behind previews, cache upkeep and share cleanup:

- **Priorities:** one queue with three priorities, highest first: share work (share pages, share listings, link previews, previews of a new or edited share), browsing (previews and thumbnails of the logged-in file browser), maintenance (cache sweeps, share cleanup, cache clearing). A queued task moves up when a higher priority asks for it.
- **Parallel workers:** 4 by default (`TASKS_WORKERS`), one reserved for share work, maintenance one at a time. ffmpeg, ffprobe and exiftool run at the CPU and disk priority of their task (`nice`, `ionice`), so the kernel serves share work first.
- **Item locks:** every task names the files or folders it works on; two tasks never run on overlapping items (a folder covers everything inside it), a held back task waits in the queue. Submitting a key that is queued or running joins it.
- **Safety:** cooldowns per key, timeouts, panic guards, bounded queues that drop upkeep work instead of growing.
- **Status:** a status file rewritten every 15 seconds (`TASKS_STATUS_FILE`) lists what is queued and every running task with its worker and items; `dpm/deploy.sh --status` prints it.

### Web client and API

- **No deletions over HTTP.** Every file and folder DELETE route of the REST API and the web client returns 403, and the Delete actions are removed from the web client file browser. SFTP deletions and share uploads are unaffected.
- **Admin user page.** The fork-only user settings live on the user page only (not in the template block shared with the group page).

### Deployment (`dpm/`)

Everything needed to deploy lives in `dpm/`, documented in [dpm/README.md](dpm/README.md). Host-specific values and secrets stay in `dpm/.env`, which git ignores; `dpm/.env.example` lists every setting.

- `dpm/docker-compose.yml` builds the image (`sftpgo-dpm:latest`) from this repository.
- `dpm/deploy.sh` backs up the database, keeps the running image for rollback, builds, tests the schema migration on a copy of the database, restarts and runs health checks. `--rollback` goes back, optionally restoring a database backup.
- The admin UI (port 8081) listens only on the VPN address; the HTTPS binding on 443 serves the web client only. Port 80 is published for ACME certificate renewal.
- `dpm/scripts/renew-cert-fallback.sh` renews the certificate over TLS-ALPN-01 when the built-in renewal fails; `dpm/scripts/apply-extensions.py` pushes `dpm/allowed-extensions.conf` to all users.

### Keeping up with upstream

- Rebase only the fork commits onto upstream `main`: `git rebase --onto upstream/main <old merge-base>`. A plain `git rebase <tag>` replays upstream development commits.
- Track upstream `main`, not release tags: this deployment's database schema is already newer than the latest release (schema 36 versus 33 for v2.7.6), and releases refuse to open it.
- Deploy with `dpm/deploy.sh`: it tests schema migrations on a copy of the database, backs it up and keeps the previous image for rollback.
- Fork code lives in its own files where possible (`sharepreview.go`, `sharehidden.go`) to keep rebases clean. Unit tests for the fork features are in `internal/httpd/internal_test.go` (`TestGetDefaultShareName`, `TestSharePreview*`, `TestHideDotEntriesInShares`).

---

# SFTPGo

[![CI Status](https://github.com/drakkan/sftpgo/workflows/CI/badge.svg)](https://github.com/drakkan/sftpgo/workflows/CI/badge.svg)
[![License: AGPL-3.0-only](https://img.shields.io/badge/License-AGPLv3-blue.svg)](https://www.gnu.org/licenses/agpl-3.0)
[![Mentioned in Awesome Go](https://awesome.re/mentioned-badge.svg)](https://github.com/avelino/awesome-go)

Full-featured and highly configurable event-driven file transfer solution. Server protocols: SFTP, HTTP/S, FTP/S, WebDAV. Storage backends: local filesystem, encrypted local filesystem, S3 (compatible) Object Storage, Google Cloud Storage, Azure Blob Storage, other SFTP servers.

With SFTPGo you can leverage local and cloud storage backends for exchanging and storing files internally or with business partners using the same tools and processes you are already familiar with.

## Project Status & Editions

SFTPGo is an open-source project with a sustainable business model. We offer two editions to suit different requirements, ensuring the project remains healthy and maintained for everyone.

### Open Source (Community)

Free, Copyleft (AGPLv3), Community Supported. The Community edition is a fully functional, production-ready solution widely adopted worldwide. It includes all the core protocols, storage backends, and the WebAdmin/WebClient UIs. It is ideal for:

- Standard file transfer needs.
- Integrating storage backends (S3, GCS, Azure Blob) with legacy protocols.
- Projects that are comfortable with AGPLv3 licensing.

### SFTPGo Enterprise

Commercial License, Professional Support, ISO 27001 Vendor. The Enterprise edition is built on the same core but extends it for mission-critical environments, compliance-heavy industries, and advanced workflows. It is a drop-in replacement (seamless upgrade).

| Feature | Open Source (Community) | Enterprise Edition |
| :--- | :--- | :--- |
| **License Type** | AGPLv3 (Copyleft) | **Commercial License**<br/>Proprietary/No Copyleft |
| **Vendor Compliance** | Not Applicable<br/>Community Project | **Certified Vendor**<br/>ISO 27001 & Supply Chain Validation |
| **Support** | Community (GitHub) | **Direct from Authors** |
| **Cloud Storage Engine** | Standard | **High Performance & Scalable**<br/>In-memory streaming (no local temp files) and up to 70% faster |
| **High Availability (HA)** | Standard<br/>Shared DB & Storage | **Advanced**<br/>Enhanced event handling and optimized instance coordination |
| **Automation Logic** | Simple Placeholders | **Dynamic Logic & Virtual Folders**<br/>Conditions, loops, route data across storage backends |
| **Data Lifecycle** | Delete / Retain | **Smart Archiving**<br/>Move data to external Cloud/SFTP storage via Virtual Folders |
| **Email Data Ingestion** | - | **Native IMAP Integration**<br/>Auto-extract attachments from email to storage |
| **Public Sharing** | Standard Links | **Advanced & Collaborative**<br/>Email Authentication & Group Delegation |
| **Data Protection** | - | **Encryption & Scanning**<br/>Automated PGP, Antivirus & DLP via ICAP |
| **Advanced Identity (SSO)** | Standard | **Extended Controls**<br/>Advanced Single Sign-On parameters |
| **Document Editing** | - | **Included**<br/>View, edit, and co-author in browser |

**Note**: We are committed to keeping the Open Source edition powerful and maintained. The Enterprise edition helps fund the development of the entire SFTPGo ecosystem.

## Sponsors

If you rely on SFTPGo in your projects, consider becoming a [sponsor](https://github.com/sponsors/drakkan).

Your sponsorship helps cover maintenance, security updates and ongoing development of the open-source edition.

### Thank you to our sponsors

#### Platinum sponsors

[<img src="./img/Aledade_logo.png" alt="Aledade logo" width="202" height="70">](https://www.aledade.com/)
</br></br>
[<img src="./img/jumptrading.png" alt="Jump Trading logo" width="198" height="85">](https://www.jumptrading.com/)
</br></br>
[<img src="./img/wpengine.png" alt="WP Engine logo" width="331" height="63">](https://wpengine.com/)

#### Silver sponsors

[<img src="./img/IDCS.png" alt="IDCS logo" width="212" height="51">](https://idcs.ip-paris.fr/)

#### Bronze sponsors

[<img src="./img/servinga.png" alt="servinga logo" width="258" height="56">](https://servinga.com/)
</br></br>
[<img src="./img/reui.png" alt="ReUI logo" width="151" height="56">](https://www.reui.io/)

## Documentation

You can explore all supported features and configuration options at [docs.sftpgo.com](https://docs.sftpgo.com/latest/).

**Note:** The link above refers to the **Community Edition**.
For details on **Enterprise Edition**, please refer to the [Enterprise Documentation](https://docs.sftpgo.com/enterprise/).

## Support

- **Community Support**: use [GitHub Discussions](https://github.com/drakkan/sftpgo/discussions) to ask questions, share feedback, and engage with other users.
- **Commercial Support**: If you require guaranteed SLAs, expert guidance, or the advanced features listed above, check out [SFTPGo Enterprise](https://sftpgo.com).

SFTPGo Enterprise is available as:

- On-premises: Full control on your infrastructure. More details: [sftpgo.com/on-premises](https://sftpgo.com/on-premises)
- Fully managed SaaS: We handle the infrastructure. More details: [sftpgo.com/saas](https://sftpgo.com/saas)

## Internationalization

The translations are available via [Crowdin](https://crowdin.com/project/sftpgo), who have granted us an open source license.

Before translating please take a look at our contribution [guidelines](https://docs.sftpgo.com/latest/web-interfaces/#internationalization).

## Release Cadence

SFTPGo follows a feature-driven release cycle.

- Enterprise Edition: Receives major new features first and follows a faster [release cadence](https://docs.sftpgo.com/enterprise/changelog/).
- Community Edition: Remains maintained, receiving bug fixes, security updates, and updates to core features.

## Acknowledgements

SFTPGo makes use of the third party libraries listed inside [go.mod](./go.mod).

We are very grateful to all the people who contributed with ideas and/or pull requests.

Thank you to [ysura](https://www.ysura.com/) for granting us stable access to a test AWS S3 account.

Thank you to [KeenThemes](https://keenthemes.com/) for granting us a custom license to use their amazing [themes](https://keenthemes.com/bootstrap-templates) for the SFTPGo WebAdmin and WebClient user interfaces, across both the Open Source and Open Core versions.

Thank you to [Crowdin](https://crowdin.com/) for granting us an Open Source License.

Thank you to [Incode](https://www.incode.it/) for helping us to improve the UI/UX.

## License

SFTPGo source code is licensed under the GNU AGPL-3.0-only with [additional terms](./NOTICE).

The [theme](https://keenthemes.com/bootstrap-templates) used in WebAdmin and WebClient user interfaces is proprietary, this means:

- KeenThemes HTML/CSS/JS components are allowed for use only within the SFTPGo product and restricted to be used in a resealable HTML template that can compete with KeenThemes products anyhow.
- The SFTPGo WebAdmin and WebClient user interfaces (HTML, CSS and JS components) based on this theme are allowed for use only within the SFTPGo product and therefore cannot be used in derivative works/products without an explicit grant from the [SFTPGo Team](mailto:support@sftpgo.com).

More information about [compliance](https://sftpgo.com/compliance.html).

**Note:** We do not provide legal advice. If you have questions about license compliance or whether your use case is permitted under the license terms, please consult your legal team.

## Copyright

Copyright (C) 2019 - 2026 Nicola Murino
