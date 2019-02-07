//
// GeoIP worker.  Takes events, looks up IP address in GeoIP database, and
// adds location information to the event.  Updated events are transmitted on
// the output queue.
//
// Worker spawns a goroutine which mainly sleeps, and periodically runs
// geoipupdate to update the GeoIP database.
//

package main

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/oschwald/geoip2-golang"
	dt "github.com/trustnetworks/analytics-common/datatypes"
	"github.com/trustnetworks/analytics-common/utils"
	"github.com/trustnetworks/analytics-common/worker"
	"golang.org/x/net/context"
)

const (

	// Program name, used for log entries.
	pgm = "geoip"

	// How often to update GeoIP data.
	updatePeriod = 86400 * time.Second
)

// Goroutine: GeoIP updater.  Periodically runs geoipupdate.
func updater(notif chan bool) {

	var waitTime = updatePeriod

	for {

		// Wait appropriate sleep period.
		time.Sleep(waitTime)

		utils.Log("Running GeoIP update...")

		// Create geoipupdate command.
		cmd := exec.Command("geoipupdate", "-f", "GeoIP.conf",
			"-d", ".")

		// Execute, stdout/stderr to byte array.
		out, err := cmd.CombinedOutput()
		if err != nil {
			utils.Log("Update error: %s", err.Error())
			utils.Log("geoipupdate: %s", out)

			// Failed: Retry sooner than the long period.
			waitTime = 60 * time.Second
			continue

		}

		utils.Log("GeoIP updated, success.")

		// On successful update, wait period is a long period.
		waitTime = updatePeriod

		// Ping the main goroutine, so it knows to reopen the
		// GeoIP database.
		notif <- true

	}

}

type work struct {

	// GeoIP City database
	geoipCityFilename string
	cityDB            *geoip2.Reader

	// GeoIP ASN database
	geoipASNFilename string
	asnDB            *geoip2.Reader

	notif chan bool
}

// Open GeoIP databases.
func (s *work) openGeoIP() {

	// No errors, but doesn't return until database is open

	for {

		// Open database.
		cityDB, err := geoip2.Open(s.geoipCityFilename)

		// If ok...
		if err == nil {
			// ...store database handle and return.
			s.cityDB = cityDB
			break
		}

		// Open failed, wait for a while and retry.
		utils.Log("Couldn't open GeoIP City database: %s", err.Error())
		time.Sleep(time.Second * 10)

		// Loop round to retry.

	}

	for {

		// Open database.
		asnDB, err := geoip2.Open(s.geoipASNFilename)

		// If ok...
		if err == nil {
			// ...store database handle and return.
			s.asnDB = asnDB
			break
		}

		// Open failed, wait for a while and retry.
		utils.Log("Couldn't open GeoIP ASN database: %s", err.Error())
		time.Sleep(time.Second * 10)

		// Loop round to retry.

	}
}

// Initialisation
func (s *work) init(notif chan bool) error {

	s.notif = notif

	// Database filenames are environment variables.
	s.geoipCityFilename = utils.Getenv("GEOIP_DB", "GeoLite2-City.mmdb")
	s.geoipASNFilename = utils.Getenv("GEOIP_ASN_DB", "GeoLite2-ASN.mmdb")

	// Open databases.
	s.openGeoIP()

	return nil

}

// GeoIP lookup
func (s *work) lookup(addr string) (*dt.Place, error) {

	// Convert IP address (string) to native form.
	ip := net.ParseIP(addr)
	if ip == nil {
		return nil, nil
	}

	// Lookup in GeoIP database.
	city, err := s.cityDB.City(ip)
	if err != nil {
		return nil, err
	}

	// If nil return, give up.
	if city == nil {
		return nil, nil
	}

	// Lookup in ASN database
	asn, err := s.asnDB.ASN(ip)
	if err != nil {
		return nil, err
	}

	// If nil return, give up.
	if asn == nil {
		return nil, nil
	}

	// Get data from GeoIP record.
	locn := &dt.Place{}
	locn.City = city.City.Names["en"]
	locn.IsoCode = city.Country.IsoCode
	locn.Country = city.Country.Names["en"]
	locn.Position = &dt.Posn{}
	locn.Position.Latitude = city.Location.Latitude
	locn.Position.Longitude = city.Location.Longitude
	locn.AccuracyRadius = int(city.Location.AccuracyRadius)
	locn.PostCode = city.Postal.Code
	locn.ASNum = asn.AutonomousSystemNumber
	locn.ASOrg = asn.AutonomousSystemOrganization

	// Don't return an empty record.
	if locn.City == "" && locn.IsoCode == "" && locn.Country == "" &&
		locn.Position.Latitude == 0.0 &&
		locn.Position.Longitude == 0.0 &&
		locn.AccuracyRadius == 0 && locn.PostCode == "" {
		return nil, nil
	}

	// Return the complete record.
	return locn, nil

}

// Event handler for new events.
func (h *work) Handle(msg []uint8, w *worker.Worker) error {

	// If there's a signal from the GeoIP database updater, re-open the
	// database.
	select {
	case _ = <-h.notif:
		utils.Log("An update occured - reopening database.")
		h.openGeoIP()

	default:
		// No signal, do nothing.

	}

	// Read event, decode JSON.
	var event dt.Event
	err := json.Unmarshal(msg, &event)
	if err != nil {
		utils.Log("Couldn't unmarshal json: %s", err.Error())
		return nil
	}

	// Debug: Dump message on output if device is 'debug'.
	if event.Device == "debug" {
		utils.Log("%s", string(msg))
	}

	var src, dest string

	// Get source IP address.
	// This gets the first address, and searches once it is found.
	// Assumption is that outer IP address is the globally addressable one
	// for GeoIP.
	for _, v := range event.Src {
		if strings.HasPrefix(v, "ipv4:") || strings.HasPrefix(v, "ipv6:") {
			src = v[5:]
			break
		}
	}

	// Get destination IP address.
	// This gets the first address, and searches once it is found.
	// Assumption is that outer IP address is the globally addressable one
	// for GeoIP.
	for _, v := range event.Dest {
		if strings.HasPrefix(v, "ipv4:") || strings.HasPrefix(v, "ipv6:") {
			dest = v[5:]
			break
		}
	}

	// Get location information from IP addresses.
	srcLoc, _ := h.lookup(src)
	destLoc, _ := h.lookup(dest)

	// If we get either a source or destination location, store the
	// information in the event record.
	if srcLoc != nil || destLoc != nil {
		event.Location = &dt.LocationInfo{}
		event.Location.Src = srcLoc
		event.Location.Dest = destLoc
	}

	// Convert event record back to JSON.
	j, err := json.Marshal(event)
	if err != nil {
		utils.Log("JSON marshal error: %s", err.Error())
		return nil
	}

	// Forward event record to output queue.
	w.Send("output", j)

	return nil

}

func main() {
	utils.LogPgm = pgm

	// Notification channel.  A bool gets sent down the channel every time
	// the updater goroutine inovkes an update.
	notif := make(chan bool, 2)

	// Launch updater goroutine
	go updater(notif)

	var w worker.QueueWorker
	var s work

	// Initialise.
	err := s.init(notif)
	if err != nil {
		utils.Log("init: %s", err.Error())
		return
	}

	// Initialise.
	var input string
	var output []string

	if len(os.Args) > 0 {
		input = os.Args[1]
	}
	if len(os.Args) > 2 {
		output = os.Args[2:]
	}

	// context to handle control of subroutines
	ctx := context.Background()
	ctx, cancel := utils.ContextWithSigterm(ctx)
	defer cancel()

	err = w.Initialise(ctx, input, output, pgm)
	if err != nil {
		utils.Log("init: %s", err.Error())
		return
	}

	utils.Log("Initialisation complete.")

	// Invoke Wye event handling.
	err = w.Run(ctx, &s)
	if err != nil {
		utils.Log("error: Event handling failed with err: %s", err.Error())
	}
}
