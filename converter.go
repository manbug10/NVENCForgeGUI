// NVENCForgeGUI (https://github.com/burnersen/NVENCForgeGUI)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

// converter.go — die mitgebrachte NVENCForge.exe finden, prüfen und notfalls
// von GitHub laden.
//
// Die Kette ist bewusst so gebaut: Der Nutzer lädt nur dieses Fenster, das
// Fenster holt sich NVENCForge.exe, und NVENCForge.exe holt sich beim ersten
// Start selbst FFmpeg. Jede Stufe kümmert sich um genau das, was sie kennt.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// converterExeName: ab-av1 kommt als Archiv ("ab-av1-windows-x86_64.zip"),
	// in dem die ausführbare Datei nur "ab-av1.exe" heißt. locateConverter und
	// der Download suchen beide nach diesem Namen im tools-Ordner.
	converterExeName = "ab-av1.exe"
	// converterAssetName ist der Name der angehängten Datei im GitHub-Release
	// von alexheretic/ab-av1. downloadConverter lädt genau dieses Asset und
	// packt daraus ab-av1.exe aus (siehe unzipSingleFile).
	converterAssetName = "ab-av1-windows-x86_64.zip"
	toolsDirName       = "tools"

	// versionFileName merkt sich, welche Ausgabe geladen wurde. Die exe selbst
	// nach ihrer Version zu fragen hieße, sie zu starten — dafür ist eine reine
	// Anzeige kein guter Grund.
	versionFileName = "ab-av1.version"

	// latestReleaseURL liefert die zuletzt veröffentlichte Ausgabe.
	latestReleaseURL = "https://api.github.com/repos/alexheretic/ab-av1/releases/latest"

	// ---- Fähigkeits-Marker (ab-av1) -------------------------------------
	//
	// Wie bisher wird die Fähigkeit aus der Binary selbst gelesen, ohne sie
	// zu starten. ab-av1 baut seine Befehle mit clap; die Long-Flags stehen
	// als Klartext in der exe. Weil alle Releases dieselbe CLI besitzen,
	// schlagen die Prüfungen bei jedem offiziellen Build an.

	// stepCommandMarker zeigt an, dass die exe den Befehl "step" kennt — den
	// eigentlichen Encoder-Durchlauf von ab-av1 (VMAF-Suche + Konvertierung).
	stepCommandMarker = "step"

	// sampleCommandMarker zeigt an, dass die exe den Befehl "sample" kennt:
	// der Vorschau-/Prüflauf, der nur misst und nichts endgültig umwandelt.
	sampleCommandMarker = "sample"

	// selectCommandMarker zeigt an, dass die exe den Befehl "select" kennt:
	// die Stapel-Auswahl nach Quelle/Filter.
	selectCommandMarker = "select"

	// convertCommandMarker zeigt an, dass die exe den Befehl "convert" kennt
	// (Konvertierung ohne VMAF-Suche, fester Encoder).
	convertCommandMarker = "convert"

	// vmafResolutionsMarker zeigt an, dass die exe --vmaf-resolutions kennt
	// (ab 0.7.0): Auflösungsfenster für die VMAF-Berechnung.
	vmafResolutionsMarker = "--vmaf-resolutions"

	// audioMarkerFlag zeigt an, dass die exe --audio-marker-map kennt (ab
	// 0.7.1): Wasserzeichen-Erkennung im Ton.
	audioMarkerFlag = "--audio-marker-map"

	downloadTimeout = 10 * time.Minute
	apiTimeout      = 30 * time.Second
)

// ConverterStatus ist alles, was die Oberfläche über die Programmdatei weiß.
type ConverterStatus struct {
	Found          bool   `json:"found"`
	Path           string `json:"path"`
	SizeBytes      int64  `json:"sizeBytes"`
	Version        string `json:"version"`
	HasStep        bool   `json:"hasStep"`
	HasSample      bool   `json:"hasSample"`
	HasSelect      bool   `json:"hasSelect"`
	HasConvert     bool   `json:"hasConvert"`
	HasVmafResol   bool   `json:"hasVmafResolutions"`
	HasAudioMarker bool   `json:"hasAudioMarkerMap"`
	ToolsDir       string `json:"toolsDir"`
	FFmpegPresent  bool   `json:"ffmpegPresent"`
	Note           string `json:"note"`
}

// toolsDirCandidates nennt die Orte, an denen die Programmdatei liegen kann.
//
// Zuerst neben der eigenen exe — so ist das fertige Programm aufgebaut. Danach
// im Arbeitsverzeichnis, denn beim Entwickeln mit "wails dev" läuft das Fenster
// aus einem Bau-Ordner, der bei jedem Start neu entsteht.
func toolsDirCandidates() []string {
	var dirs []string
	add := func(dir string) {
		for _, existing := range dirs {
			if strings.EqualFold(existing, dir) {
				return
			}
		}
		dirs = append(dirs, dir)
	}
	if exe, err := os.Executable(); err == nil {
		add(filepath.Join(filepath.Dir(exe), toolsDirName))
	}
	if wd, err := os.Getwd(); err == nil {
		add(filepath.Join(wd, toolsDirName))
	}
	return dirs
}

// locateConverter sucht die Programmdatei an allen bekannten Orten.
func locateConverter() (string, bool) {
	for _, dir := range toolsDirCandidates() {
		candidate := filepath.Join(dir, converterExeName)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, true
		}
	}
	return "", false
}

// downloadDir bestimmt, wohin eine geladene Programmdatei gehört: in einen
// bereits vorhandenen tools-Ordner, sonst in den ersten möglichen.
func downloadDir() (string, error) {
	candidates := toolsDirCandidates()
	if len(candidates) == 0 {
		return "", fmt.Errorf("converter.go: downloadDir: no writable location found")
	}
	for _, dir := range candidates {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir, nil
		}
	}
	return candidates[0], nil
}

// converterStatus stellt zusammen, was die Oberfläche anzeigen soll.
func converterStatus() ConverterStatus {
	dir, _ := downloadDir()
	status := ConverterStatus{ToolsDir: dir}

	path, found := locateConverter()
	if !found {
		status.Note = "ab-av1.exe is not here yet — it can be downloaded from GitHub."
		return status
	}

	status.Found = true
	status.Path = path
	status.ToolsDir = filepath.Dir(path)
	if info, err := os.Stat(path); err == nil {
		status.SizeBytes = info.Size()
	}
	status.Version = readVersionFile(filepath.Dir(path))
	// FFmpeg/ffprobe liegen bei ab-av1 neben der exe oder im PATH; die
	// Erstausstattung (setup.go) lädt sie wie bisher herunter.
	status.FFmpegPresent = fileExists(filepath.Join(filepath.Dir(path), "ffmpeg.exe"))

	// Fähigkeitsprüfung: Befehle und Flags direkt aus der Binary lesen, ohne
	// sie zu starten. Ein Lesefehler zählt als "kann es nicht": lieber einen
	// Knopf zu wenig als einen, der nichts tut.
	hasStep, err := fileContainsMarker(path, stepCommandMarker)
	switch {
	case err != nil:
		status.Note = "Could not read ab-av1.exe: " + err.Error()
	case hasStep:
		status.HasStep = true
	default:
		status.Note = "This ab-av1.exe does not provide the 'step' command. " +
			"The run still works, but progress can only be read from the log."
	}
	if hasSample, e := fileContainsMarker(path, sampleCommandMarker); e == nil && hasSample {
		status.HasSample = true
	}
	if hasSelect, e := fileContainsMarker(path, selectCommandMarker); e == nil && hasSelect {
		status.HasSelect = true
	}
	if hasConvert, e := fileContainsMarker(path, convertCommandMarker); e == nil && hasConvert {
		status.HasConvert = true
	}
	if hasVmafRes, e := fileContainsMarker(path, vmafResolutionsMarker); e == nil && hasVmafRes {
		status.HasVmafResol = true
	}
	if hasAudioMarker, e := fileContainsMarker(path, audioMarkerFlag); e == nil && hasAudioMarker {
		status.HasAudioMarker = true
	}
	return status
}

// readVersionFile liefert die gemerkte Ausgabe oder einen ehrlichen Ersatz.
func readVersionFile(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, versionFileName))
	if err != nil {
		return "local build"
	}
	version := strings.TrimSpace(string(raw))
	if version == "" {
		return "local build"
	}
	return version
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// fileContainsMarker sucht eine Zeichenkette in einer Datei, ohne sie komplett
// in den Speicher zu holen.
//
// Der Übertrag am Pufferende ist der Grund für den Umweg: Ohne ihn würde ein
// Treffer übersehen, der genau auf einer Blockgrenze liegt.
func fileContainsMarker(path, marker string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("converter.go: fileContainsMarker (Open): %w", err)
	}
	defer file.Close()

	needle := []byte(marker)
	overlap := len(needle) - 1
	const blockSize = 1 << 20

	buffer := make([]byte, blockSize+overlap)
	carried := 0
	for {
		read, readErr := file.Read(buffer[carried:])
		if read > 0 {
			window := buffer[:carried+read]
			if bytes.Contains(window, needle) {
				return true, nil
			}
			carried = overlap
			if carried > len(window) {
				carried = len(window)
			}
			copy(buffer, window[len(window)-carried:])
		}
		if readErr == io.EOF {
			return false, nil
		}
		if readErr != nil {
			return false, fmt.Errorf("converter.go: fileContainsMarker (Read): %w", readErr)
		}
	}
}

// ----------------------------------------------------------------------------
// Herunterladen von GitHub
// ----------------------------------------------------------------------------

type releaseAsset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"browser_download_url"`
}

type releaseInfo struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

// fetchLatestRelease fragt GitHub nach der neuesten Veröffentlichung des
// Konverters.
func fetchLatestRelease(ctx context.Context) (releaseInfo, error) {
	return fetchRelease(ctx, latestReleaseURL)
}

// fetchRelease fragt ein beliebiges GitHub-Verzeichnis nach einer
// Veröffentlichung.
//
// Der Umweg über die Adresse als Übergabewert ist der Grund, warum das
// Selbst-Update (selfupdate.go) dieselbe Leitung benutzen kann: Zwei Kopien
// dieser Abfrage würden früher oder später auseinanderlaufen.
func fetchRelease(ctx context.Context, url string) (releaseInfo, error) {
	var release releaseInfo

	requestCtx, cancel := context.WithTimeout(callContext(ctx), apiTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, url, nil)
	if err != nil {
		return release, fmt.Errorf("converter.go: fetchRelease (NewRequest): %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "NVENCForgeGUI")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return release, fmt.Errorf("converter.go: fetchRelease (Do): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return release, fmt.Errorf("converter.go: fetchRelease: GitHub answered %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return release, fmt.Errorf("converter.go: fetchRelease (Decode): %w", err)
	}
	return release, nil
}

// callContext hält einen fehlenden Zusammenhang ab.
//
// Nötig, weil der Zusammenhang des Fensters erst beim Start entsteht: Ein
// Aufruf davor käme mit nil an, und context.WithTimeout stürzt damit ab.
func callContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// pickConverterAsset sucht das Windows-Archiv von ab-av1 unter den angehängten
// Dateien des Releases. Aus ihm wird nach dem Download ab-av1.exe ausgepackt
// (siehe unzipSingleFile).
func pickConverterAsset(release releaseInfo) (releaseAsset, error) {
	return pickAsset(release, converterAssetName)
}

// unzipSingleFile packt genau eine Datei aus einem ZIP-Archiv an den
// Zielort. ab-av1 liefert seine exe im Zip; die übrigen Einträge des Archivs
// sind für dieses Fenster ohne Wert und bleiben deshalb liegen.
func unzipSingleFile(zipPath, memberName, targetPath string) error {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("converter.go: unzipSingleFile (OpenReader): %w", err)
	}
	defer reader.Close()

	for _, f := range reader.File {
		// Im Archiv kann der Pfad einen Ordner enthalten (z.B. "bin/ab-av1.exe").
		base := filepath.ToSlash(filepath.Base(f.Name))
		if !strings.EqualFold(base, memberName) || f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("converter.go: unzipSingleFile (Open): %w", err)
		}
		defer rc.Close()

		out, err := os.Create(targetPath)
		if err != nil {
			return fmt.Errorf("converter.go: unzipSingleFile (Create): %w", err)
		}
		if _, err := io.Copy(out, rc); err != nil {
			out.Close()
			_ = os.Remove(targetPath)
			return fmt.Errorf("converter.go: unzipSingleFile (Copy): %w", err)
		}
		if err := out.Close(); err != nil {
			_ = os.Remove(targetPath)
			return fmt.Errorf("converter.go: unzipSingleFile (Close): %w", err)
		}
		return nil
	}
	return fmt.Errorf("converter.go: unzipSingleFile: archive has no %q", memberName)
}

// pickAsset sucht eine angehängte Datei nach ihrem Namen.
func pickAsset(release releaseInfo, name string) (releaseAsset, error) {
	for _, asset := range release.Assets {
		if strings.EqualFold(asset.Name, name) {
			return asset, nil
		}
	}
	return releaseAsset{}, fmt.Errorf(
		"converter.go: pickAsset: release %s has no %s attached", release.TagName, name)
}

// progressWriter meldet den Fortschritt weiter, während geschrieben wird.
type progressWriter struct {
	total    int64
	done     int64
	lastSent time.Time
	report   func(done, total int64)
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.done += int64(len(p))
	// Gedrosselt, sonst erzeugt ein 9-MB-Download tausende Meldungen an die
	// Oberfläche — dieselbe Überlegung wie beim Fortschritt des Konverters.
	if w.report != nil && time.Since(w.lastSent) >= 100*time.Millisecond {
		w.lastSent = time.Now()
		w.report(w.done, w.total)
	}
	return len(p), nil
}

// downloadToFile holt eine angehängte Datei an den angegebenen Platz und meldet
// dabei den Fortschritt.
//
// Geschrieben wird genau dorthin, wohin der Aufrufer zeigt; dass das eine
// Teildatei ist und was danach mit ihr geschieht, entscheidet er. Scheitert der
// Download, bleibt nichts liegen — eine halbe Datei, die niemand mehr aufräumt,
// wäre schlimmer als gar keine.
func downloadToFile(ctx context.Context, asset releaseAsset, targetPath string, report func(done, total int64)) error {
	downloadCtx, cancel := context.WithTimeout(callContext(ctx), downloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return fmt.Errorf("converter.go: downloadToFile (NewRequest): %w", err)
	}
	req.Header.Set("User-Agent", "NVENCForgeGUI")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("converter.go: downloadToFile (Do): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("converter.go: downloadToFile: download answered %s", resp.Status)
	}

	file, err := os.Create(targetPath)
	if err != nil {
		return fmt.Errorf("converter.go: downloadToFile (Create): %w", err)
	}

	counter := &progressWriter{total: asset.Size, report: report}
	_, copyErr := io.Copy(io.MultiWriter(file, counter), resp.Body)
	closeErr := file.Close()

	if copyErr != nil {
		_ = os.Remove(targetPath)
		return fmt.Errorf("converter.go: downloadToFile (Copy): %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(targetPath)
		return fmt.Errorf("converter.go: downloadToFile (Close): %w", closeErr)
	}

	if report != nil {
		report(asset.Size, asset.Size)
	}
	return nil
}

// DownloadResult sagt der Oberfläche, was der Download bewirkt hat.
type DownloadResult struct {
	Status   ConverterStatus `json:"status"`
	Replaced bool            `json:"replaced"`
	Tag      string          `json:"tag"`
	Message  string          `json:"message"`
}

// downloadConverter lädt die neueste ab-av1.exe in den tools-Ordner.
//
// ab-av1 veröffentlicht seine Windows-Binary als ZIP-Archiv. Geladen wird das
// Archiv zuerst in eine Teildatei; erst nach vollständigem Download wird
// daraus ab-av1.exe ausgepackt und eingesetzt. Sonst bliebe nach einem Abbruch
// eine halbe Datei liegen, die beim nächsten Start als vorhanden gälte.
//
// force=false schützt vor einem Rückschritt: Kann die vorhandene Programmdatei
// den 'step'-Befehl und die geladene nicht, wird NICHT ersetzt. Fortschritt
// und Ergebnisse hängen bei ab-av1 an genau diesem Befehl.
func downloadConverter(ctx context.Context, force bool, report func(done, total int64)) (DownloadResult, error) {
	before := converterStatus()
	result := DownloadResult{Status: before}

	release, err := fetchLatestRelease(ctx)
	if err != nil {
		return result, err
	}
	result.Tag = release.TagName

	asset, err := pickConverterAsset(release)
	if err != nil {
		return result, err
	}

	dir, err := downloadDir()
	if err != nil {
		return result, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return result, fmt.Errorf("converter.go: downloadConverter (MkdirAll): %w", err)
	}

	targetPath := filepath.Join(dir, converterExeName)
	partZipPath := filepath.Join(dir, converterAssetName+".part")
	defer os.Remove(partZipPath) // im Erfolgsfall ist sie schon weg; hier nur der Restschutz

	if err := downloadToFile(ctx, asset, partZipPath, report); err != nil {
		return result, err
	}

	// Auspacken in eine Teildatei neben dem Ziel, damit ein Fehler im Zip nie
	// die funktionierende exe überschreibt.
	partExePath := targetPath + ".part"
	if err := unzipSingleFile(partZipPath, converterExeName, partExePath); err != nil {
		return result, err
	}
	_ = os.Remove(partZipPath)

	if !force && wouldLoseStepCommand(before, partExePath) {
		_ = os.Remove(partExePath)
		result.Message = "Release " + release.TagName + " has no 'step' command, " +
			"but the converter installed here has one. Nothing was replaced — " +
			"the progress display would have stopped working."
		return result, nil
	}

	// Eine laufende alte exe kann nicht überschrieben werden; die vorher zu
	// entfernen ist der verlässlichere Weg als sich auf Rename zu verlassen.
	_ = os.Remove(targetPath)
	if err := os.Rename(partExePath, targetPath); err != nil {
		return result, fmt.Errorf("converter.go: downloadConverter (Rename): %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, versionFileName), []byte(release.TagName), 0o644); err != nil {
		// Die Programmdatei liegt richtig — nur die Versionsnotiz fehlt. Das ist
		// kein Grund, den Download als gescheitert zu melden.
		_ = err
	}

	// Die frisch eingespielte Fassung einmal kurz anstoßen, damit sie ihre
	// Konfigurationsdatei (ab-av1.toml) auf den eigenen Stand bringt und —
	// wie bisher — FFmpeg lädt, falls es fehlt. Ohne das passiert es erst bei
	// der nächsten Konvertierung.
	primeConverter(ctx, targetPath)

	result.Replaced = true
	result.Status = converterStatus()
	result.Message = "ab-av1 " + release.TagName + " installed."
	return result, nil
}

// primeTimeout begrenzt den Anstoß nach dem Einspielen. Gemessen braucht ein
// Leerlauf des Konverters etwa eine Sekunde; eine halbe Minute liegt also weit
// jenseits von allem Normalen und dient nur dazu, dass ein hängender Prozess
// nicht unbemerkt im Hintergrund stehen bleibt. Die Erstausstattung hat
// bewusst keine solche Grenze — dort kann ein FFmpeg-Download dazwischenliegen.
const primeTimeout = 30 * time.Second

// primeConverter stößt die eben eingespielte exe einmal an, damit sie ihre
// Konfigurationsdatei auf den Stand der neuen Ausgabe bringt.
//
// Warum das nötig ist: Der Konverter trägt fehlende Einstellungen beim START
// nach, nicht beim Einspielen. Ohne Anstoß erschiene ein neuer Schlüssel erst
// nach der nächsten Konvertierung — dieses Fenster baut seine
// Einstellungsseite aber aus genau dieser Datei. (Die exe sonst NICHT zu
// starten ist Absicht, siehe versionFileName weiter oben; eine bloße Anzeige
// rechtfertigt das nicht, eine unvollständige Einstellungsseite schon.)
//
// Wie der Lauf abgesichert ist, steht bei runConverterIdle — es ist derselbe
// Weg, den auch die Erstausstattung geht. Fehler bleiben bewusst folgenlos:
// Klappt der Anstoß nicht, gilt wieder das Verhalten von vor 1.11.0, und die
// Einstellungen kommen beim nächsten echten Lauf dazu. Das Update deswegen als
// gescheitert zu melden, wäre falsch — die Programmdatei liegt ja richtig.
func primeConverter(ctx context.Context, exePath string) {
	runCtx, cancel := context.WithTimeout(ctx, primeTimeout)
	defer cancel()
	_ = runConverterIdle(runCtx, exePath, nil)
}

// wouldLoseStepCommand prüft, ob das Einspielen ein Rückschritt wäre.
//
// Lässt sich die geladene Datei nicht lesen, gilt sie als "kein Rückschritt" —
// die Entscheidung darüber trifft dann die Prüfung nach dem Einspielen, und
// die Oberfläche sagt es ohnehin an.
func wouldLoseStepCommand(before ConverterStatus, downloadedPath string) bool {
	if !before.Found || !before.HasStep {
		return false
	}
	hasStep, err := fileContainsMarker(downloadedPath, stepCommandMarker)
	if err != nil {
		return false
	}
	return !hasStep
}
