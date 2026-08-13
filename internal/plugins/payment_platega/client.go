package payment_platega

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type PaymentDetails struct {
	Amount   int    `json:"amount"`
	Currency string `json:"currency"`
}

type Metadata struct {
	UserId   string `json:"userId"`
	UserName string `json:"userName,omitempty"`
}

type CreatePaymentRequest struct {
	PaymentDetails PaymentDetails `json:"paymentDetails"`
	Description    string         `json:"description"`
	ReturnURL      string         `json:"return"`
	FailedURL      string         `json:"failedUrl"`
	Payload        string         `json:"payload"`
	Metadata       Metadata       `json:"metadata"`
}

func generateRandomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// CreatePayment creates a new payment link via the Platega API.
func CreatePayment(merchantID, secret string, userID string, userName string, amount int, description string, returnURL string, failedURL string) (string, string, error) {
	orderID := generateRandomID()
	reqBody := CreatePaymentRequest{
		PaymentDetails: PaymentDetails{
			Amount:   amount,
			Currency: "RUB",
		},
		Description: description,
		ReturnURL:   returnURL,
		FailedURL:   failedURL,
		Payload:     orderID,
		Metadata: Metadata{
			UserId:   userID,
			UserName: userName,
		},
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		return "", "", err
	}

	if merchantID == "dummy" {
		return "https://mock.platega.com/pay/" + orderID, orderID, nil
	}

	req, err := http.NewRequest("POST", "https://app.platega.io/v2/transaction/process", bytes.NewReader(b))
	if err != nil {
		return "", "", err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-MerchantId", merchantID)
	req.Header.Set("X-Secret", secret)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	bResp, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("platega API returned status %d: %s", resp.StatusCode, string(bResp))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(bResp, &result); err != nil {
		return "", "", err
	}

	var payURL, payID string
	if dataMap, ok := result["data"].(map[string]interface{}); ok {
		if u, ok := dataMap["url"].(string); ok {
			payURL = u
		}
		if i, ok := dataMap["transactionId"].(string); ok {
			payID = i
		} else if i, ok := dataMap["id"].(string); ok {
			payID = i
		}
	} else {
		if u, ok := result["url"].(string); ok {
			payURL = u
		}
		if i, ok := result["transactionId"].(string); ok {
			payID = i
		} else if i, ok := result["id"].(string); ok {
			payID = i
		}
	}

	if payURL == "" || payID == "" {
		return "", "", fmt.Errorf("platega API did not return a valid URL or ID: %s", string(bResp))
	}

	return payURL, payID, nil
}
