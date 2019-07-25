package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/manifoldco/promptui"
	"github.com/urfave/cli"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func promptAccountSid() (string, error) {
	validate := func(input string) error {
		r, _ := regexp.Compile("^\\s*AC[0-9a-f]{32}\\s*$")
		if !r.MatchString(input) {
			return errors.New("Invalid Account SID")
		}
		return nil
	}

	prompt := promptui.Prompt{
		Label:    "Account SID",
		Validate: validate,
	}

	result, err := prompt.Run()
	return strings.TrimSpace(result), err
}

func promptAuthToken() (string, error) {
	validate := func(input string) error {
		r, _ := regexp.Compile("^\\s*[0-9a-f]{32}\\s*$")
		if !r.MatchString(input) {
			return errors.New("Invalid Auth Token")
		}
		return nil
	}

	prompt := promptui.Prompt{
		Label:    "Auth Token",
		Validate: validate,
		Mask:     '*',
	}

	result, err := prompt.Run()
	return strings.TrimSpace(result), err
}

func promptNumberSelection(numbersData [][2]string) ([2]string, error) {
	var numbers []string
	for _, number := range numbersData {
		numbers = append(numbers, number[0])
	}

	prompt := promptui.Select{
		Label: "Select Number",
		Items: numbers,
	}

	_, result, err := prompt.Run()
	if err != nil {
		return [2]string{"", ""}, err
	}

	for _, number := range numbersData {
		if number[0] == result {
			return number, nil
		}
	}
	return [2]string{"", ""}, errors.New("failed to select number")
}

func handleCallback(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		fmt.Println("Received a message but couldn't parse body")
		return
	}
	fmt.Printf("%v %v\n", r.Form.Get("From"), r.Form.Get("Body"))
}

func runServer() (int, error) {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	go func() {
		http.HandleFunc("/", handleCallback)
		panic(http.Serve(listener, nil))
	}()
	return port, nil
}

func updateNumberCallback(client http.Client, accountSid string, authToken string, sid string, callbackUrl string) error {
	_, err := client.PostForm(fmt.Sprintf("https://%v:%v@api.twilio.com/2010-04-01/Accounts/%v/IncomingPhoneNumbers/%v.json", accountSid, authToken, accountSid, sid), url.Values{
		"Sid": {sid},
		"SmsMethod": {"POST"},
		"SmsUrl": {callbackUrl},
	})
	if err != nil {
		fmt.Printf("Failed configuring the callback %v\n", err)
		return err
	}
	return nil
}

func getNumbers(client http.Client, accountSid string, authToken string) ([][2]string, error) {
	numbersReq, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%v/IncomingPhoneNumbers.json", accountSid), nil)
	if err != nil {
		return nil, err
	}
	numbersReq.SetBasicAuth(accountSid, authToken)

	r, err := client.Do(numbersReq)
	if err != nil {
		return nil, err
	}

	var phoneNumbersResponse IncomingPhoneNumbersResponse
	err = json.NewDecoder(r.Body).Decode(&phoneNumbersResponse)
	if err != nil {
		return nil, err
	}
	var capableNumbers [][2]string
	for _, number := range phoneNumbersResponse.IncomingPhoneNumbers {
		if number.Capabilities.Sms && number.Status == "in-use" {
			capableNumbers = append(capableNumbers, [2]string{number.PhoneNumber, number.Sid})
		}
	}
	return capableNumbers, nil
}

func runNgrokServer(portNum int) error {
	port := strconv.Itoa(portNum)
	cmd := exec.Command("ngrok", "http", port)
	err := cmd.Start()
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func(){
		for range c {
			err := cmd.Process.Kill()
			if err != nil {
				panic("Unable to kill ngrok")
			}
			os.Exit(0)
		}
	}()
	return err
}

func fetchNgrokUrl(client http.Client, portNum int) (string, error) {
	port := strconv.Itoa(portNum)
	r, err := client.Get("http://127.0.0.1:4040/api/tunnels")
	if err != nil {
		fmt.Println("Failed to get tunnels")
		return "", err
	}

	var tunnelsResponse TunnelsResponse
	err = json.NewDecoder(r.Body).Decode(&tunnelsResponse)
	if err != nil {
		fmt.Println("Failed to decode tunnels")
		return "", err
	}
	var url string
	for _, tunnel := range tunnelsResponse.Tunnels {
		if tunnel.Proto == "https" && strings.HasSuffix(tunnel.Config.Addr, ":" + port) {
			return tunnel.PublicURL, nil
		}
	}

	return url, errors.New("couldn't find the correct Public URL")
}

func main() {


	app := cli.NewApp()
	app.Name = "twicat"
	app.Usage = "read sms from a Twilio number"

	app.Action = func(c *cli.Context) error {

		port, err := runServer()
		if err != nil {
			fmt.Printf("Unable to run server %v\n", err)
			os.Exit(1)
		}

		err = runNgrokServer(port)
		if err != nil {
			fmt.Printf("Unable to run ngrok %v\n", err)
			os.Exit(1)
		}

		accountSid, err := promptAccountSid()
		if err != nil {
			fmt.Printf("Failed to read Account SID %v\n", err)
			os.Exit(1)
		}

		authToken, err := promptAuthToken()
		if err != nil {
			fmt.Printf("Failed to read Auth Token %v\n", err)
			os.Exit(1)
		}

		client := &http.Client{Timeout: 5 * time.Second}
		numbersData, err := getNumbers(*client, accountSid, authToken)
		if err != nil {
			fmt.Printf("Failed to fetch numbers. Check your credentials. %v\n", err)
			os.Exit(1)
		}

		selectedNumber, err := promptNumberSelection(numbersData)
		if err != nil {
			fmt.Printf("Failed to select Number %v\n", err)
			os.Exit(1)
		}

		ngrokUrl, err := fetchNgrokUrl(*client, port)
		if err != nil {
			fmt.Printf("Couldn't start ngrok %v\n", err)
			os.Exit(1)
		}

		err = updateNumberCallback(*client, accountSid, authToken, selectedNumber[1], ngrokUrl)
		if err != nil {
			fmt.Printf("Couldn't set callback URL %v\n", err)
			os.Exit(1)
		}

		fmt.Println()

		for {
			time.Sleep(1 * time.Second)
		}
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}

type IncomingPhoneNumbersResponse struct {
	IncomingPhoneNumbers []IncomingPhoneNumber `json:"incoming_phone_numbers"`
}

type IncomingPhoneNumber struct {
	Sid          string       `json:"sid"`
	PhoneNumber  string       `json:"phone_number"`
	Capabilities Capabilities `json:"capabilities"`
	Status       string       `json:"status"`
}

type Capabilities struct {
	Sms bool `json:"sms"`
}

type TunnelsResponse struct {
	Tunnels []Tunnel `json:"tunnels"`
}

type Tunnel struct {
	PublicURL string `json:"public_url"`
	Config    Config `json:"config"`
	Proto     string `json:"proto"`
}

type Config struct {
	Addr string `json:"addr"`
}
