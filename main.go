package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gorilla/handlers"
	"github.com/julienschmidt/httprouter"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

type User struct {
	Email string `json:"email"`
}

type ServerConfigItem struct {
	Type string `toml:"type"`

	Host struct {
		IsSecure              bool   `toml:"secure"`
		Bind                  string `toml:"bind"`
		Port                  int    `toml:"port"`
		FQDN                  string `toml:"fqdn"`
		SSLCertificatePath    string `toml:"ssl_certificate"`
		SSLCertificateKeyPath string `toml:"ssl_certificate_key"`
		CookiePassthrough     bool   `toml:"cookie_passthrough"`
		CookieEncryptionKey   string `toml:"cookie_encryption_key"`
	} `toml:"host"`

	Target struct {
		Url string `toml:"url"`
	} `toml:"target"`

	Authentication struct {
		ClientID           string   `toml:"client_id"`
		ClientSecret       string   `toml:"client_secret"`
		Scopes             []string `toml:"scopes"`
		CookieName         string   `toml:"cookie_name"`
		CookieDurationDays int      `toml:"cookie_duration_days"`
	} `toml:"authentication"`

	Authorization struct {
		RequireAuth bool       `toml:"require_auth"`
		AllowAll    bool       `toml:"allow_all"`
		AllowList   []string   `toml:"allow_list"`
		AllowArgs   [][]string `toml:"allow_args"`
		CookieName  string     `toml:"cookie_name"`
	} `toml:"authorization"`

	OauthConfig *oauth2.Config
}

type MyTransport struct {
	http.RoundTripper
	Config ServerConfigItem
}

func (t *MyTransport) EmailHasAccess(email string) (bool, string) {
	// if you have a custom email permission check, you should do it here.
	if t.Config.Authorization.AllowAll == false {
		var hit bool = false
		for _, test_email := range t.Config.Authorization.AllowList {
			if email == test_email {
				hit = true
			}
		}
		return hit, "User Not Allowed"
	}

	return true, ""
}

func (t *MyTransport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	cookie, err := req.Cookie(t.Config.Authorization.CookieName)

	if err != nil {
		if t.Config.Authorization.RequireAuth == true {
			resp := http.Response{
				Body: ioutil.NopCloser(strings.NewReader("Auth required.")),
			}
			return &resp, nil
		} else {
			req.Header.Set("X-Peanut-Auth", "")
		}
	} else {
		email, _ := Decrypt(t.Config.Host.CookieEncryptionKey, cookie.Value)
		req.Header.Set("X-Peanut-Auth", email)

		allowed, reason := t.EmailHasAccess(email)
		if allowed == false {
			resp := http.Response{
				Body: ioutil.NopCloser(strings.NewReader(reason)),
			}
			return &resp, nil
		}
	}

	cookies := req.Cookies()
	remaining_cookies := make([]string, len(cookies))

	for _, i := range cookies {
		if i.Name == t.Config.Authentication.CookieName ||
			i.Name == t.Config.Authorization.CookieName {
			if t.Config.Host.CookiePassthrough == false {
				continue
			} else {
				newValue, _ := Decrypt(t.Config.Host.CookieEncryptionKey, i.Value)
				i.Value = base64.StdEncoding.EncodeToString([]byte(newValue))
			}
		}
		remaining_cookies = append(remaining_cookies, i.String())
	}

	req.Header.Set("Cookie", strings.Join(remaining_cookies, ";"))

	resp, err = t.RoundTripper.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	err = resp.Body.Close()
	if err != nil {
		return nil, err
	}

	body := ioutil.NopCloser(bytes.NewReader(b))

	resp.Body = body
	resp.ContentLength = int64(len(b))
	resp.Header.Set("Content-Length", strconv.Itoa(len(b)))

	return resp, nil
}

func Encrypt(key string, text string) (string, error) {
	if len(key) == 0 {
		return text, nil
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return "", err
	}
	b := base64.StdEncoding.EncodeToString([]byte(text))
	ciphertext := make([]byte, aes.BlockSize+len(b))
	iv := ciphertext[:aes.BlockSize]
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return "", err
	}
	cfb := cipher.NewCFBEncrypter(block, iv)
	cfb.XORKeyStream(ciphertext[aes.BlockSize:], []byte(b))
	bc := base64.StdEncoding.EncodeToString(ciphertext)
	return bc, nil
}

func Decrypt(key, b64text string) (string, error) {
	text, _ := base64.StdEncoding.DecodeString(b64text)
	if len(key) == 0 {
		return string(text), nil
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return "", err
	}
	if len(text) < aes.BlockSize {
		return "", errors.New("ciphertext too short")
	}
	iv := text[:aes.BlockSize]
	text = text[aes.BlockSize:]
	cfb := cipher.NewCFBDecrypter(block, iv)
	cfb.XORKeyStream(text, text)
	data, err := base64.StdEncoding.DecodeString(string(text))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func MakeCookie(key string, value string, days int) *http.Cookie {
	expiration := time.Now().AddDate(0, 0, days)

	return &http.Cookie{
		Name:    key,
		Value:   value,
		Expires: expiration,
		Path:    "/",
	}
}

func GenRandomString() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func HtmlRedirect(url string) string {
	return fmt.Sprintf("<html><meta http-equiv=\"refresh\" content=\"0;url='%s'\" /></html>", url)
}

func main() {
	done := make(chan bool, 1)

	b, err := ioutil.ReadFile("config.toml")
	if err != nil {
		panic(err)
	}

	var config struct {
		Servers []ServerConfigItem `toml:"server"`
	}

	_, err = toml.Decode(string(b), &config)

	if err != nil {
		panic(err)
	}

	for _, config := range config.Servers {
		router := httprouter.New()

		target, _ := url.Parse(config.Target.Url)
		tp := MyTransport{http.DefaultTransport, config}

		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.Transport = &tp

		config.OauthConfig = &oauth2.Config{
			ClientID:     config.Authentication.ClientID,
			ClientSecret: config.Authentication.ClientSecret,
			RedirectURL:  fmt.Sprintf("%s/_/auth", config.Host.FQDN),
			Scopes:       config.Authentication.Scopes,
			Endpoint:     google.Endpoint,
		}

		router.GET("/_/login", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
			queryValues := r.URL.Query()
			next := queryValues.Get("next")

			if len(next) > 0 {
				http.SetCookie(w, MakeCookie("next", next, 1))
			} else {
				http.SetCookie(w, MakeCookie("next", "", -101))
			}

			randomToken := GenRandomString()

			http.SetCookie(w,
				MakeCookie(
					config.Authentication.CookieName,
					randomToken,
					config.Authentication.CookieDurationDays,
				),
			)

			fmt.Fprintln(w, HtmlRedirect(config.OauthConfig.AuthCodeURL(randomToken)))
		})

		router.GET("/_/logout", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
			queryValues := r.URL.Query()
			next := queryValues.Get("next")

			http.SetCookie(w, MakeCookie(config.Authentication.CookieName, "", -101))
			http.SetCookie(w, MakeCookie(config.Authorization.CookieName, "", -101))

			if len(next) == 0 {
				fmt.Fprintf(w, "<html><body>You have been logged out.</body></html>")
			} else {
				fmt.Fprintf(w, HtmlRedirect(next))
				http.SetCookie(w, MakeCookie("next", "/", -100))
			}
		})

		router.GET("/_/auth", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
			cookie, err := r.Cookie(config.Authentication.CookieName)

			if err != nil {
				http.SetCookie(w, MakeCookie(config.Authentication.CookieName, "", -101))
				http.SetCookie(w, MakeCookie(config.Authorization.CookieName, "", -101))
				fmt.Fprintln(w, HtmlRedirect("/_/login"))
				return
			}

			queryValues := r.URL.Query()
			urlState := queryValues.Get("state")

			if urlState != cookie.Value {
				fmt.Fprintf(w, "<html><body>An error occurred.</body></html>")
				return
			}

			code := queryValues.Get("code")
			tok, terr := config.OauthConfig.Exchange(oauth2.NoContext, code)

			if terr != nil {
				panic(terr)
			}

			b, _ := json.Marshal(tok)

			enc_value, _ := Encrypt(config.Host.CookieEncryptionKey, string(b))
			http.SetCookie(w, MakeCookie(
				config.Authentication.CookieName,
				enc_value,
				config.Authentication.CookieDurationDays,
			))

			client := config.OauthConfig.Client(oauth2.NoContext, tok)

			// This part is very google dependent.
			email, err := client.Get("https://www.googleapis.com/oauth2/v3/userinfo")

			if err != nil {
				fmt.Fprintf(w, "<html><body>Invalid Token</Body></html>")
				return
			}

			defer email.Body.Close()

			data, _ := ioutil.ReadAll(email.Body)

			var user User
			json.Unmarshal(data, &user)

			ident_enc_value, _ := Encrypt(
				config.Host.CookieEncryptionKey,
				user.Email,
			)
			http.SetCookie(w, MakeCookie(
				config.Authorization.CookieName,
				ident_enc_value,
				config.Authentication.CookieDurationDays,
			))

			cookie, err = r.Cookie("next")
			if err != nil {
				fmt.Fprintf(w, HtmlRedirect("/_/test"))
				http.SetCookie(w, MakeCookie("next", "", -100))
			} else {
				url := cookie.Value
				fmt.Fprintf(w, HtmlRedirect(url))
				http.SetCookie(w, MakeCookie("next", "", -100))
			}
		})

		router.GET("/_/test", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
			cookie, err := r.Cookie(config.Authentication.CookieName)
			var tok oauth2.Token

			if err != nil {
				fmt.Fprintf(w, "<html><body>Not logged in</body></html>")
				return
			}

			decrypted_code, _ := Decrypt(config.Host.CookieEncryptionKey, cookie.Value)

			json.Unmarshal([]byte(decrypted_code), &tok)
			client := config.OauthConfig.Client(oauth2.NoContext, &tok)
			email, err := client.Get("https://www.googleapis.com/oauth2/v3/userinfo")

			if err != nil {
				log.Println(err)
				fmt.Fprintf(w, "<html><body>Invalid Token</Body></html>")
				return
			}

			defer email.Body.Close()
			data, _ := ioutil.ReadAll(email.Body)
			fmt.Fprintf(w, "<html><body>%s</body></html>", data)
		})

		router.GET("/_/landing", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
			fmt.Fprintf(w, "<html><body><a href='/_/login'>Login</a></body></html>")
		})

		router.GET("/_/add_scopes", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
			queryValues := r.URL.Query()
			scopes := strings.Split(queryValues.Get("scopes"), ",")

			scopes = append(scopes, config.Authentication.Scopes...)

			TempOauthConfig := &oauth2.Config{
				ClientID:     config.Authentication.ClientID,
				ClientSecret: config.Authentication.ClientSecret,
				RedirectURL:  fmt.Sprintf("%s/_/auth", config.Host.FQDN),
				Scopes:       scopes,
				Endpoint:     google.Endpoint,
			}

			randomToken := GenRandomString()
			http.SetCookie(w, MakeCookie(
				config.Authentication.CookieName,
				randomToken,
				config.Authentication.CookieDurationDays,
			))

			fmt.Fprintln(w, TempOauthConfig.AuthCodeURL(randomToken))
		})

		router.GET("/_/hello/:name", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
			fmt.Fprintf(w, "hello, %s!\n", ps.ByName("name"))
		})

		router.NotFound = proxy

		go func() {
			if config.Host.IsSecure == true {
				log.Fatal(http.ListenAndServeTLS(
					fmt.Sprintf("%s:%d", config.Host.Bind, config.Host.Port),
					config.Host.SSLCertificatePath,
					config.Host.SSLCertificateKeyPath,
					handlers.LoggingHandler(os.Stdout, router)),
				)
			} else {
				log.Fatal(http.ListenAndServe(
					fmt.Sprintf("%s:%d", config.Host.Bind, config.Host.Port),
					handlers.LoggingHandler(os.Stdout, router)),
				)
			}
		}()
	}

	<-done
}