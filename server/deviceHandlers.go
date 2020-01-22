package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/dexidp/dex/storage"
)

type deviceCodeResponse struct {
	//The unique device code for device authentication
	DeviceCode string `json:"device_code"`
	//The code the user will exchange via a browser and log in
	UserCode string `json:"user_code"`
	//The url to verify the user code.
	VerificationURI string `json:"verification_uri"`
	//The verification uri with the user code appended for pre-filling form
	VerificationURIComplete string `json:"verification_uri_complete"`
	//The lifetime of the device code
	ExpireTime int `json:"expires_in"`
	//How often the device is allowed to poll to verify that the user login occurred
	PollInterval int `json:"interval"`
}

func (s *Server) getDeviceAuthURI() string {
	return path.Join(s.issuerURL.Path, "/device/auth/verify_code")
}

func (s *Server) handleDeviceExchange(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		userCode := r.URL.Query().Get("user_code")
		invalidAttempt, err := strconv.ParseBool(r.URL.Query().Get("invalid"))
		if err != nil {
			invalidAttempt = false
		}
		if err := s.templates.device(r, w, s.getDeviceAuthURI(), userCode, invalidAttempt); err != nil {
			s.logger.Errorf("Server template error: %v", err)
		}
	default:
		s.renderError(r, w, http.StatusBadRequest, "Requested resource does not exist.")
	}
}

func (s *Server) handleDeviceCode(w http.ResponseWriter, r *http.Request) {
	//TODO replace with configurable values
	//expireIntervalSeconds := s.deviceRequestsValidFor
	pollIntervalSeconds := 5

	switch r.Method {
	case http.MethodPost:
		err := r.ParseForm()
		if err != nil {
			message := fmt.Sprintf("Could not parse Device Request body: %v", err)
			s.logger.Errorf(message)
			s.tokenErrHelper(w, errInvalidRequest, message, http.StatusBadRequest)
			return
		}

		//Get the client id and scopes from the post
		clientID := r.Form.Get("client_id")
		scopes := r.Form["scope"]

		s.logger.Infof("Received device request for client %v with scopes %v", clientID, scopes)

		//Make device code
		deviceCode := storage.NewDeviceCode()

		//make user code
		userCode := storage.NewUserCode()

		//Generate the expire time
		//expireTime := time.Now().Add(time.Second * time.Duration(expireIntervalSeconds))
		expireTime := time.Now().Add(s.deviceRequestsValidFor)

		//Store the Device Request
		deviceReq := storage.DeviceRequest{
			UserCode:   userCode,
			DeviceCode: deviceCode,
			ClientID:   clientID,
			Scopes:     scopes,
			Expiry:     expireTime,
		}

		if err := s.storage.CreateDeviceRequest(deviceReq); err != nil {
			s.logger.Errorf("Failed to store device request; %v", err)
			s.tokenErrHelper(w, errServerError, "Could not create device request", http.StatusInternalServerError)
			return
		}

		//Store the device token
		deviceToken := storage.DeviceToken{
			DeviceCode:          deviceCode,
			Status:              deviceTokenPending,
			Expiry:              expireTime,
			LastRequestTime:     time.Now(),
			PollIntervalSeconds: 5,
		}

		if err := s.storage.CreateDeviceToken(deviceToken); err != nil {
			s.logger.Errorf("Failed to store device token %v", err)
			s.tokenErrHelper(w, errServerError, "Could not create device token", http.StatusInternalServerError)
			return
		}

		u, err := url.Parse(s.issuerURL.String())
		if err != nil {
			s.logger.Errorf("Could not parse issuer URL %v", err)
			s.renderError(r, w, http.StatusInternalServerError, "")
			return
		}
		u.Path = path.Join(u.Path, "device")
		vURI := u.String()

		q := u.Query()
		q.Set("user_code", userCode)
		u.RawQuery = q.Encode()
		vURIComplete := u.String()

		code := deviceCodeResponse{
			DeviceCode:              deviceCode,
			UserCode:                userCode,
			VerificationURI:         vURI,
			VerificationURIComplete: vURIComplete,
			ExpireTime:              int(s.deviceRequestsValidFor.Seconds()),
			PollInterval:            pollIntervalSeconds,
		}

		enc := json.NewEncoder(w)
		enc.SetIndent("", "   ")
		enc.Encode(code)

	default:
		s.renderError(r, w, http.StatusBadRequest, "Invalid device code request type")
		s.tokenErrHelper(w, errInvalidRequest, "Invalid device code request type", http.StatusBadRequest)
	}
}

func (s *Server) handleDeviceToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodPost:
		err := r.ParseForm()
		if err != nil {
			message := "Could not parse Device Token Request body"
			s.logger.Warnf("%s : %v", message, err)
			s.tokenErrHelper(w, errInvalidRequest, message, http.StatusBadRequest)
			return
		}

		deviceCode := r.Form.Get("device_code")
		if deviceCode == "" {
			message := "No device code received"
			s.tokenErrHelper(w, errInvalidRequest, message, http.StatusBadRequest)
			return
		}

		grantType := r.PostFormValue("grant_type")
		if grantType != grantTypeDeviceCode {
			s.tokenErrHelper(w, errInvalidGrant, "Unsupported grant type.  Must be device_code", http.StatusBadRequest)
			return
		}

		now := s.now()

		//Grab the device token from the db
		deviceToken, err := s.storage.GetDeviceToken(deviceCode)
		if err != nil || now.After(deviceToken.Expiry) {
			if err != storage.ErrNotFound {
				s.logger.Errorf("failed to get device code: %v", err)
				s.tokenErrHelper(w, errServerError, "Device code not found", http.StatusInternalServerError)
			} else {
				s.tokenErrHelper(w, deviceTokenExpired, "Invalid or expired device code parameter.", http.StatusBadRequest)
			}
			return
		}

		//Rate Limiting check
		pollInterval := deviceToken.PollIntervalSeconds
		minRequestTime := deviceToken.LastRequestTime.Add(time.Second * time.Duration(pollInterval))
		if now.Before(minRequestTime) {
			s.tokenErrHelper(w, deviceTokenSlowDown, "", http.StatusBadRequest)
			//Continually increase the poll interval until the user waits the proper time
			pollInterval += 5
		} else {
			pollInterval = 5
		}

		switch deviceToken.Status {
		case deviceTokenPending:
			updater := func(old storage.DeviceToken) (storage.DeviceToken, error) {
				old.PollIntervalSeconds = pollInterval
				old.LastRequestTime = now
				return old, nil
			}
			// Update device token last request time in storage
			if err := s.storage.UpdateDeviceToken(deviceCode, updater); err != nil {
				s.logger.Errorf("failed to update device token: %v", err)
				s.renderError(r, w, http.StatusInternalServerError, "")
				return
			}
			s.tokenErrHelper(w, deviceTokenPending, "", http.StatusUnauthorized)
		case deviceTokenComplete:
			w.Write([]byte(deviceToken.Token))
		}
	default:
		s.renderError(r, w, http.StatusBadRequest, "Requested resource does not exist.")
	}
}

func (s *Server) handleDeviceCallback(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		userCode := r.FormValue("state")
		code := r.FormValue("code")

		if userCode == "" || code == "" {
			s.renderError(r, w, http.StatusBadRequest, "Request was missing parameters")
			return
		}

		// Authorization redirect callback from OAuth2 auth flow.
		if errMsg := r.FormValue("error"); errMsg != "" {
			http.Error(w, errMsg+": "+r.FormValue("error_description"), http.StatusBadRequest)
			return
		}

		authCode, err := s.storage.GetAuthCode(code)
		if err != nil || s.now().After(authCode.Expiry) {
			if err != storage.ErrNotFound {
				s.logger.Errorf("failed to get auth code: %v", err)
			}
			s.renderError(r, w, http.StatusBadRequest, "Invalid or expired auth code.")
			return
		}

		//Grab the device request from the db
		deviceReq, err := s.storage.GetDeviceRequest(userCode)
		if err != nil || s.now().After(deviceReq.Expiry) {
			if err != storage.ErrNotFound {
				s.logger.Errorf("failed to get device code: %v", err)
			}
			s.renderError(r, w, http.StatusInternalServerError, "Invalid or expired device code.")
			return
		}

		reqClient, err := s.storage.GetClient(deviceReq.ClientID)
		if err != nil {
			s.logger.Errorf("Failed to get reqClient %q: %v", deviceReq.ClientID, err)
			s.renderError(r, w, http.StatusInternalServerError, "Failed to retrieve device client.")
			return
		}

		resp, err := s.exchangeAuthCode(w, authCode, reqClient)
		if err != nil {
			s.logger.Errorf("Could not exchange auth code for client %q: %v", deviceReq.ClientID, err)
			s.renderError(r, w, http.StatusInternalServerError, "Failed to exchange auth code.")
			return
		}

		//Grab the device request from the db
		old, err := s.storage.GetDeviceToken(deviceReq.DeviceCode)
		if err != nil || s.now().After(old.Expiry) {
			if err != storage.ErrNotFound {
				s.logger.Errorf("failed to get device token: %v", err)
			}
			s.renderError(r, w, http.StatusInternalServerError, "Invalid or expired device code.")
			return
		}

		updater := func(old storage.DeviceToken) (storage.DeviceToken, error) {
			if old.Status == deviceTokenComplete {
				return old, errors.New("device token already complete")
			}
			respStr, err := json.MarshalIndent(resp, "", "  ")
			if err != nil {
				s.logger.Errorf("failed to marshal device token response: %v", err)
				s.renderError(r, w, http.StatusInternalServerError, "")
				return old, err
			}

			old.Token = string(respStr)
			old.Status = deviceTokenComplete
			return old, nil
		}

		// Update refresh token in the storage, store the token and mark as complete
		if err := s.storage.UpdateDeviceToken(deviceReq.DeviceCode, updater); err != nil {
			s.logger.Errorf("failed to update device token: %v", err)
			s.renderError(r, w, http.StatusInternalServerError, "")
			return
		}

		if err := s.templates.deviceSuccess(r, w, reqClient.Name); err != nil {
			s.logger.Errorf("Server template error: %v", err)
		}

	default:
		http.Error(w, fmt.Sprintf("method not implemented: %s", r.Method), http.StatusBadRequest)
		return
	}
}

func (s *Server) verifyUserCode(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		err := r.ParseForm()
		if err != nil {
			message := "Could not parse user code verification Request body"
			s.logger.Warnf("%s : %v", message, err)
			s.tokenErrHelper(w, errInvalidRequest, message, http.StatusBadRequest)
			return
		}

		userCode := r.Form.Get("user_code")
		if userCode == "" {
			s.renderError(r, w, http.StatusBadRequest, "No user code received")
			return
		}

		userCode = strings.ToUpper(userCode)

		//Find the user code in the available requests
		deviceRequest, err := s.storage.GetDeviceRequest(userCode)
		if err != nil || s.now().After(deviceRequest.Expiry) {
			if err != storage.ErrNotFound {
				s.logger.Errorf("failed to get device request: %v", err)
				s.tokenErrHelper(w, errServerError, "", http.StatusInternalServerError)
			}
			if err := s.templates.device(r, w, s.getDeviceAuthURI(), userCode, true); err != nil {
				s.logger.Errorf("Server template error: %v", err)
			}
			return
		}

		//Redirect to Dex Auth Endpoint
		authURL := path.Join(s.issuerURL.Path, "/auth")
		u, err := url.Parse(authURL)
		if err != nil {
			s.renderError(r, w, http.StatusInternalServerError, "Invalid auth URI.")
			return
		}
		q := u.Query()
		q.Set("client_id", deviceRequest.ClientID)
		q.Set("state", deviceRequest.UserCode)
		q.Set("response_type", "code")
		q.Set("redirect_uri", path.Join(s.issuerURL.Path, "/device/callback"))
		q.Set("scope", strings.Join(deviceRequest.Scopes, " "))
		u.RawQuery = q.Encode()

		http.Redirect(w, r, u.String(), http.StatusFound)

	default:
		s.renderError(r, w, http.StatusBadRequest, "Requested resource does not exist.")
	}
}
