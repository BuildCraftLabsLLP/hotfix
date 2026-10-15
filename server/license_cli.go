package main

// `hotfixd license …`. The CLI talks to the license server itself and writes
// license.json; the running service adopts the change within seconds.

import (
	"errors"
	"fmt"
	"io"
	"time"
)

func licenseCommand(args []string, w io.Writer) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	if sub == "status" {
		return printLicenseStatus(w)
	}
	if !licenseEnforced() {
		return errors.New("this is an unlicensed development build; licensing is off")
	}
	pub := licensePubKey()
	if pub == nil {
		return errors.New("this build carries an invalid license public key")
	}
	if err := needElevation(); err != nil {
		return err
	}
	cfg, err := readConfig(configPath())
	if err != nil {
		return err
	}
	st := loadLicenseState()
	if _, _, err := st.identity(); err != nil {
		return err
	}

	switch sub {
	case "activate":
		if len(args) != 2 {
			return errors.New("usage: hotfixd license activate <HFXS-… key>")
		}
		if st.SeatID != "" {
			if err := st.deactivate(cfg); err != nil {
				return fmt.Errorf("releasing this server's current seat: %w", err)
			}
		}
		c, err := st.activate(cfg, args[1], pub)
		if err != nil {
			return err
		}
		if err := saveLicenseState(st); err != nil {
			return err
		}
		fmt.Fprintf(w, "Activated: this server now holds a seat on the license (%s).\n", c.SeatID)
		printModeHint(w, cfg)
		return nil

	case "deactivate":
		if st.SeatID == "" {
			fmt.Fprintln(w, "This server holds no seat.")
			return nil
		}
		if err := st.deactivate(cfg); err != nil {
			return err
		}
		if err := saveLicenseState(st); err != nil {
			return err
		}
		fmt.Fprintln(w, "Seat released. This server is back on its trial (if any days are left); otherwise it only reports.")
		return nil

	case "refresh":
		c, _, err := st.refresh(cfg, pub)
		if isAPIError(err, "seat_gone") {
			st.SeatID, st.SeatSecret, st.Token = "", "", ""
			_ = saveLicenseState(st)
			return errors.New("this server's seat is gone (released, or taken by a machine with the same identity); run `hotfixd license activate <key>`")
		}
		if err != nil {
			return err
		}
		if err := saveLicenseState(st); err != nil {
			return err
		}
		fmt.Fprintf(w, "Refreshed: %s.\n", describeClaims(c))
		return nil

	case "reset-identity":
		// For cloned machines that share another's identity: a new instance id
		// (the old seat belongs to the original machine, so it's left alone).
		st.InstanceID, st.SeatID, st.SeatSecret, st.Token = "", "", "", ""
		if _, _, err := st.identity(); err != nil {
			return err
		}
		if err := saveLicenseState(st); err != nil {
			return err
		}
		fmt.Fprintln(w, "This server has a new identity. Activate it with `hotfixd license activate <key>`.")
		return nil
	}
	return fmt.Errorf("unknown license command %q (status, activate, deactivate, refresh, reset-identity)", sub)
}

func printModeHint(w io.Writer, cfg Config) {
	if cfg.Mode != ModeEnforce {
		fmt.Fprintln(w, "Hotfix is in monitor mode (it only reports). Turn on killing with: hotfixd config set mode enforce")
	}
}

func describeClaims(c licenseClaims) string {
	switch {
	case c.Status == "active" && c.Plan == "trial":
		return fmt.Sprintf("free trial, %d days left", trialDaysLeft(c.TrialEnd))
	case c.Status == "active":
		return "licensed (paid subscription)"
	case c.Plan == "trial_expired":
		return "trial ended: not licensed"
	default:
		return "subscription not active: not licensed"
	}
}

// printLicenseStatus reads the service's view (status.json) when available,
// else the stored token; no network, no elevation needed.
func printLicenseStatus(w io.Writer) error {
	if !licenseEnforced() {
		fmt.Fprintln(w, "License:   none needed (unlicensed development build)")
		return nil
	}
	if st, err := readStatus(); err == nil && time.Since(st.Updated) < time.Minute {
		fmt.Fprintf(w, "License:   %s\n", describeSummary(st.License))
		return nil
	}
	fmt.Fprintln(w, "License:   unknown (the service isn't running; run `sudo hotfixd license status` while it is, or `hotfixd license refresh`)")
	return nil
}

func describeSummary(s licenseSummary) string {
	switch {
	case s.Plan == "dev":
		return "none needed (development build)"
	case s.Licensed && s.Plan == "trial":
		return fmt.Sprintf("free trial, %d days left (buy: https://hotfix.buildcraft.town/server.html)", trialDaysLeft(s.TrialEnd))
	case s.Licensed:
		return "licensed"
	case s.Note != "":
		return "NOT licensed: " + s.Note + ". Monitoring only."
	default:
		return "NOT licensed. Monitoring only."
	}
}
