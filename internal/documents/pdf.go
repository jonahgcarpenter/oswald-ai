package documents

import (
	"context"
	"errors"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// Profile policy is defense in depth, not a guarantee against external resource
// access. Writer link/field/chart updates are disabled; common security also
// disables macros and requires trusted referers for link updates in all modules.
const officeProfile = `<?xml version="1.0" encoding="UTF-8"?>
<oor:items xmlns:oor="http://openoffice.org/2001/registry">
<item oor:path="/org.openoffice.Office.Common/Security/Scripting">
<prop oor:name="MacroSecurityLevel" oor:op="fuse"><value>3</value></prop>
<prop oor:name="DisableMacrosExecution" oor:op="fuse"><value>true</value></prop>
<prop oor:name="SecureURL" oor:op="fuse"><value/></prop>
<prop oor:name="BlockUntrustedRefererLinks" oor:op="fuse"><value>true</value></prop>
<prop oor:name="ExecutePlugins" oor:op="fuse"><value>false</value></prop>
</item>
<item oor:path="/org.openoffice.Office.Writer/Content/Update">
<prop oor:name="Link" oor:op="fuse"><value>2</value></prop>
<prop oor:name="Field" oor:op="fuse"><value>false</value></prop>
<prop oor:name="Chart" oor:op="fuse"><value>false</value></prop>
</item>
</oor:items>`

func (e *Extractor) convert(ctx context.Context, dir, input, ext string) (string, error) {
	profile := filepath.Join(dir, "profile")
	if err := os.MkdirAll(filepath.Join(profile, "user"), 0700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(profile, "user", "registrymodifications.xcu"), []byte(officeProfile), 0600); err != nil {
		return "", err
	}
	filter := "pdf:writer_pdf_Export"
	if ext == ".ppt" || ext == ".pptx" || ext == ".odp" {
		filter = "pdf:impress_pdf_Export"
	}
	profileURL := (&url.URL{Scheme: "file", Path: profile}).String()
	// Pin the import filter too: do not let a mislabeled file select Calc or
	// another importer with different active-content behavior.
	importFilter := map[string]string{".doc": "MS Word 97", ".docx": "Office Open XML Text", ".odt": "writer8", ".rtf": "Rich Text Format", ".ppt": "MS PowerPoint 97", ".pptx": "Impress MS PowerPoint 2007 XML", ".odp": "impress8"}[ext]
	if importFilter == "" {
		return "", ErrUnsupported
	}
	args := []string{"-env:UserInstallation=" + profileURL, "--headless", "--nologo", "--nodefault", "--nofirststartwizard", "--norestore", "--infilter=" + importFilter, "--convert-to", filter, "--outdir", dir, input}
	_, err := e.run(ctx, dir, libreoffice, args...)
	// soffice.bin requests one restart after first-use profile initialization.
	// Handle its launcher protocol directly, without invoking the shell wrapper.
	var exit interface {
		error
		ExitCode() int
	}
	if errors.As(err, &exit) && exit.ExitCode() == 81 && ctx.Err() == nil {
		_, err = e.run(ctx, dir, libreoffice, args...)
	}
	if err != nil {
		return "", err
	}
	output := filepath.Join(dir, "input.pdf")
	s, err := os.Lstat(output)
	if err != nil {
		return "", err
	}
	if !s.Mode().IsRegular() || s.Size() == 0 {
		return "", ErrInvalid
	}
	if s.Size() > maxFile {
		return "", ErrLimit
	}
	return output, nil
}

func (e *Extractor) pdf(ctx context.Context, dir, input string, converted bool, r *Result) error {
	info, err := e.run(ctx, dir, pdfinfo, input)
	if err != nil {
		return err
	}
	pages := 0
	for _, line := range strings.Split(string(info), "\n") {
		if strings.HasPrefix(line, "Pages:") {
			pages, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Pages:")))
		}
	}
	if pages < 1 {
		return ErrInvalid
	}
	if pages > maxPages {
		pages = maxPages
		r.Partial = true
	}
	ocr := 0
	for page := 1; page <= pages; page++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := strconv.Itoa(page)
		text, err := e.run(ctx, dir, pdftotext, "-f", p, "-l", p, "-layout", "-enc", "UTF-8", "-nopgbrk", input, "-")
		if err != nil {
			return err
		}
		method := "native"
		letters := 0
		for _, c := range string(text) {
			if unicode.IsLetter(c) || unicode.IsDigit(c) {
				letters++
			}
		}
		if letters < 40 {
			if ocr >= maxOCRPages {
				r.Partial = true
			} else {
				ocr++
				base := filepath.Join(dir, "page")
				_, err = e.run(ctx, dir, pdftoppm, "-f", p, "-l", p, "-singlefile", "-scale-to", "3000", "-gray", "-png", input, base)
				if err != nil {
					appendText(r, string(text), pageLocator(page), pdfMethod(converted, method))
					return err
				}
				image := base + ".png"
				if err = validatePageImage(image); err == nil {
					var recognized []byte
					recognized, err = e.run(ctx, dir, tesseract, image, "stdout", "-l", "eng", "--psm", "3")
					if err == nil && strings.TrimSpace(string(recognized)) != "" {
						text = recognized
						method = "ocr"
					}
				}
				_ = os.Remove(image)
				if err != nil {
					appendText(r, string(text), pageLocator(page), pdfMethod(converted, method))
					return err
				}
			}
		}
		if !appendText(r, strings.TrimSpace(string(text)), pageLocator(page), pdfMethod(converted, method)) {
			if page < pages {
				r.Partial = true
			}
			break
		}
	}
	return nil
}

func pdfMethod(converted bool, method string) string {
	if converted {
		return "libreoffice-" + method
	}
	return method
}

func validatePageImage(path string) error {
	s, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !s.Mode().IsRegular() {
		return ErrInvalid
	}
	if s.Size() > maxFile {
		return ErrLimit
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	c, err := png.DecodeConfig(f)
	if err != nil {
		return ErrInvalid
	}
	if c.Width < 1 || c.Height < 1 || c.Width > 3000 || c.Height > 3000 || int64(c.Width)*int64(c.Height) > 9000000 {
		return ErrLimit
	}
	return nil
}
