package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cloudflare/cloudflare-go"
	mapset "github.com/deckarep/golang-set/v2"
	"golang.org/x/sys/unix"
	"tailscale.com/client/tailscale"
	"tailscale.com/ipn/ipnstate"
)

const (
	CloudflareSyncDNSComment = "_tailscale"
	CloudflareDomainSuffix   = ".int"
	SyncInternal             = 30 * time.Second
)

var (
	ctx    context.Context
	lc     tailscale.LocalClient
	st     *ipnstate.Status
	api    *cloudflare.API
	zoneID string
	stop   context.CancelFunc
)

func init() {
	signal.Reset(syscall.SIGTERM, syscall.SIGINT)
	ctx, stop = signal.NotifyContext(context.Background(), unix.SIGTERM, unix.SIGINT)

	var err error
	// init ts local client
	st, err = lc.Status(ctx)
	if err != nil {
		panic(err)
	}
	// init cloudflare client
	api, err = cloudflare.NewWithAPIToken(os.Getenv("CLOUDFLARE_TOKEN"))
	if err != nil {
		panic(err)
	}
	// get zone id
	zoneID, err = api.ZoneIDByName(os.Getenv("CLOUDFLARE_DOMAIN"))
	if err != nil {
		panic(err)
	}
}

func getName(name string) string {
	// normalize name
	s := strings.Split(strings.ToLower(name), ".")
	if len(s) > 0 {
		return s[0]
	}
	return ""
}

func sync(ctx context.Context) {
	log.Printf("sync start")
	// name => ip string
	tsMap := map[string]string{}
	ts := mapset.NewSet[string]()
	// name => record id
	cfMap := map[string]string{}
	cf := mapset.NewSet[string]()
	// add self name
	{
		name := getName(st.Self.DNSName)
		if name != "" {
			ts.Add(name)
			// now only support ipv4
			for _, ip := range st.Self.TailscaleIPs {
				if ip.Is4() {
					tsMap[name] = ip.String()
				}
			}
		}
	}
	// add peer name
	for _, ps := range st.Peer {
		name := getName(ps.DNSName)
		if name != "" {
			ts.Add(name)
			// now only support ipv4
			for _, ip := range ps.TailscaleIPs {
				if ip.Is4() {
					tsMap[name] = ip.String()
				}
			}
		}
	}
	records, _, err := api.ListDNSRecords(ctx, cloudflare.ZoneIdentifier(zoneID), cloudflare.ListDNSRecordsParams{
		Comment: CloudflareSyncDNSComment,
		ResultInfo: cloudflare.ResultInfo{
			// cloudflare limit 1000 records per page
			PerPage: 1000,
		},
	})
	if err != nil {
		log.Printf("ListDNSRecords: %+v", err)
		return
	}
	for _, r := range records {
		name := getName(r.Name)
		if name != "" {
			cf.Add(getName(r.Name))
			cfMap[name] = r.ID
		}
	}
	needToSync := ts.SymmetricDifference(cf).ToSlice()
	if len(needToSync) == 0 {
		log.Printf("no host need to sync")
		return
	}
	for _, name := range ts.SymmetricDifference(cf).ToSlice() {
		if ts.Contains(name) {
			ip, ok := tsMap[name]
			if !ok {
				continue
			}
			log.Printf("%s need to add to cf", name)
			_, err := api.CreateDNSRecord(ctx, cloudflare.ZoneIdentifier(zoneID), cloudflare.CreateDNSRecordParams(cloudflare.DNSRecord{
				Type:    "A",
				Name:    fmt.Sprintf("%s"+CloudflareDomainSuffix, name),
				Content: ip,
				Comment: CloudflareSyncDNSComment,
				TTL:     1,
			}))
			if err != nil {
				log.Printf("CreateDNSRecord: %+v", err)
				continue
			}
			log.Printf("%s added to cf", name)
		}
		if cf.Contains(name) {
			recordID, ok := cfMap[name]
			if !ok {
				continue
			}
			log.Printf("%s need to remove from cf", name)
			err := api.DeleteDNSRecord(ctx, cloudflare.ZoneIdentifier(zoneID), recordID)
			if err != nil {
				log.Printf("DeleteDNSRecord: %+v", err)
				continue
			}
			log.Printf("%s removed from cf", name)
		}
	}
	log.Printf("sync end")
}

func main() {
	defer stop()
	ticker := time.NewTicker(SyncInternal)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			sync(ctx)
			ticker.Reset(SyncInternal)
		case <-ctx.Done():
			log.Println("sync stopped")
			return
		}
	}
}
