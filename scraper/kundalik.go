package scraper

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

func TakeGradeScreenshot(login, password string) ([]byte, error) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-setuid-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("window-size", "1280,900"),
		chromedp.UserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"),
	)

	// Agar Linux tizimi (Render/Docker) bo'lsa, Chromium manzilini o'rnatamiz
	if runtime.GOOS == "linux" {
		chromePath := os.Getenv("CHROME_BIN")
		if chromePath == "" {
			chromePath = "/usr/bin/chromium"
		}
		opts = append(opts, chromedp.ExecPath(chromePath))
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAlloc()

	ctx, cancelCtx := chromedp.NewContext(allocCtx)
	defer cancelCtx()

	ctx, cancelTimeout := context.WithTimeout(ctx, 45*time.Second)
	defer cancelTimeout()

	var buf []byte

	// Brauzer bot ekanini yashirish (Stealth script)
	stealthScript := `
		Object.defineProperty(navigator, 'webdriver', { get: () => undefined });
		window.chrome = { runtime: {} };
	`

	loginURL := "https://login.emaktab.uz/"

	err := chromedp.Run(ctx,
		chromedp.ActionFunc(func(c context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(stealthScript).Do(c)
			return err
		}),
		emulation.SetDeviceMetricsOverride(1280, 900, 1.0, false),

		// 1. Kirish sahifasini ochish
		chromedp.Navigate(loginURL),
		chromedp.WaitVisible(`input[name="login"]`, chromedp.ByQuery),
		chromedp.Sleep(1*time.Second),

		// 2. JavaScript orqali xavfsiz qiymat berish va hodisalarni chaqirish (Input Event)
		chromedp.Evaluate(fmt.Sprintf(`(() => {
			let logInput = document.querySelector('input[name="login"]');
			if (logInput) {
				logInput.focus();
				logInput.value = %q;
				logInput.dispatchEvent(new Event('input', { bubbles: true }));
				logInput.dispatchEvent(new Event('change', { bubbles: true }));
			}
			let passInput = document.querySelector('input[name="password"]');
			if (passInput) {
				passInput.focus();
				passInput.value = %q;
				passInput.dispatchEvent(new Event('input', { bubbles: true }));
				passInput.dispatchEvent(new Event('change', { bubbles: true }));
			}
		})()`, login, password), nil),

		chromedp.Sleep(500*time.Millisecond),

		// 3. Kirish tugmasini bosish
		chromedp.Click(`input[type="submit"], button[type="submit"]`, chromedp.ByQuery),
		chromedp.Sleep(4*time.Second),

		// 4. Baholar sahifasiga o'tish
		chromedp.Navigate("https://emaktab.uz/marks"),
		chromedp.Sleep(4*time.Second),

		// 5. Skrinshot olish
		chromedp.CaptureScreenshot(&buf),
	)

	if err != nil {
		return nil, fmt.Errorf("skrinshot xatosi: %w", err)
	}

	if len(buf) == 0 {
		return nil, fmt.Errorf("bo'sh rasm qaytdi")
	}

	return buf, nil
}