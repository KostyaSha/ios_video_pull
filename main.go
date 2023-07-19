package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/danielpaulus/quicktime_video_hack/screencapture"
	"github.com/google/gousb"

	"go.nanomsg.org/mangos/v3"
	"go.nanomsg.org/mangos/v3/protocol/push"
	// register transports
	_ "github.com/nanomsg/mangos/transport/all"

	log "github.com/sirupsen/logrus"
)

func main() {
	var udid = flag.String("udid", "", "Device UDID")
	var devicesCmd = flag.Bool("devices", false, "List devices then exit")
	var decimalFlag = flag.Bool("decimal", false, "Show product ID / vendor ID in decimal")
	var jsonFlag = flag.Bool("json", false, "Use JSON output when listing devices")
	var pullCmd = flag.Bool("pull", false, "Pull video")
	var pushSpec = flag.String("pushSpec", "tcp://127.0.0.1:7878", "NanoMsg spec to push h264 nalus to")
	var file = flag.String("file", "", "File to save h264 nalus into")
	var verbose = flag.Bool("v", false, "Verbose Debugging")
	var enableCmd = flag.Bool("enable", false, "Enable device")
	var disableCmd = flag.Bool("disable", false, "Disable device")
	flag.Parse()

	log.SetFormatter(&log.JSONFormatter{})

	if *verbose {
		log.Info("Set Debug mode")
		log.SetLevel(log.DebugLevel)
	}

	if *devicesCmd {
		devices(*jsonFlag, *udid, *decimalFlag)
		return
	} else if *pullCmd {
		gopull(*pushSpec, *file, *udid)
	} else if *enableCmd {
		enable(*udid)
	} else if *disableCmd {
		disable(*udid)
	} else {
		flag.Usage()
	}
}

func devices(useJson bool, udid string, useDecimal bool) {
	ctx := gousb.NewContext()

	devs, err := findIosDevices(ctx)
	if err != nil {
		log.Errorf("Error finding iOS Devices - %s", err)
	}

	if useJson && udid == "" {
		fmt.Printf("[\n")
	}
	for _, dev := range devs {
		serial, _ := dev.SerialNumber()
		if udid != "" && serial != udid {
			continue
		}
		product, _ := dev.Product()
		subcs := getVendorSubclasses(dev.Desc)
		activated := 0
		for _, subc := range subcs {
			if int(subc) == 42 {
				activated = 1
			}
		}
		pidHex := dev.Desc.Product.String()
		vidHex := dev.Desc.Vendor.String()
		pid := ""
		vid := ""
		if useDecimal {
			pid64, _ := strconv.ParseInt(pidHex, 16, 16)
			vid64, _ := strconv.ParseInt(vidHex, 16, 16)
			pid = strconv.Itoa(int(pid64))
			vid = strconv.Itoa(int(vid64))
		} else {
			pid = pidHex
			vid = vidHex
		}
		if useJson {
			name := strings.Replace(product, `"`, `\"`, -1)
			fmt.Printf(`{"bus":%d,"addr":%d,"port":%d,"udid":"%s","name":"%s","vid":"%s","pid":"%s","activated":%d}`,
				dev.Desc.Bus, dev.Desc.Address, dev.Desc.Port, serial, name, vid, pid, activated)
			if udid == "" {
				fmt.Printf(",")
			}
			fmt.Printf("\n")
		} else {
			fmt.Printf("Bus: %d, Address: %d, Port: %d, UDID:%s, Name:%s, VID=%s, PID=%s, Activated=%d\n", dev.Desc.Bus, dev.Desc.Address, dev.Desc.Port, serial, product, vid, pid, activated)
		}
		dev.Close()
	}
	if useJson && udid == "" {
		fmt.Printf("]\n")
	}

	ctx.Close()
}

func openDevice(ctx *gousb.Context, uuid string) (*gousb.Device, bool) {
	devs, err := findIosDevices(ctx)
	if err != nil {
		log.Errorf("Error finding iOS Devices - %s", err)
	}
	var foundDevice *gousb.Device = nil
	activated := false
	for _, dev := range devs {
		serial, _ := dev.SerialNumber()
		if serial == uuid {
			foundDevice = dev
			subcs := getVendorSubclasses(dev.Desc)
			for _, subc := range subcs {
				if int(subc) == 42 {
					activated = true
				}
			}
		} else {
			dev.Close()
		}
	}
	return foundDevice, activated
}

func findIosDevices(ctx *gousb.Context) ([]*gousb.Device, error) {
	return ctx.OpenDevices(func(dev *gousb.DeviceDesc) bool {
		for _, subc := range getVendorSubclasses(dev) {
			if subc == gousb.ClassApplication {
				return true
			}
		}
		return false
	})
}

func getVendorSubclasses(desc *gousb.DeviceDesc) []gousb.Class {
	subClasses := []gousb.Class{}
	for _, conf := range desc.Configs {
		for _, iface := range conf.Interfaces {
			if iface.AltSettings[0].Class == gousb.ClassVendorSpec {
				subClass := iface.AltSettings[0].SubClass
				subClasses = append(subClasses, subClass)
			}
		}
	}
	return subClasses
}

func gopull(pushSpec string, filename string, udid string) {
	stopChannel := make(chan interface{})
	stopChannel2 := make(chan interface{})
	stopChannel3 := make(chan bool)
	waitForSigInt(stopChannel, stopChannel2, stopChannel3)

	var fh *os.File
	var err error
	var writer screencapture.CmSampleBufConsumer
	if filename == "" {
		pushSock := setup_nanomsg_sockets(pushSpec)
		writer = NewNanoWriter(pushSock)
	} else {
		fh, err = os.Create(filename)
		if err != nil {
			log.Errorf("Error creating file %s:%s", filename, err)
		}
		writer = NewFileWriter(bufio.NewWriter(fh))
	}

	attempt := 1
	for {
		success := startWithConsumer(writer, udid, stopChannel, stopChannel2)
		if success {
			break
		}
		fmt.Printf("Attempt %i to start streaming\n", attempt)
		if attempt >= 4 {
			log.WithFields(log.Fields{
				"type": "stream_start_failed",
			}).Fatal("Socket new error")
		}
		attempt++
		time.Sleep(time.Second * 1)
	}

	<-stopChannel3
	writer.Stop()
}

func setup_nanomsg_sockets(pushSpec string) (pushSock mangos.Socket) {
	var err error
	if pushSock, err = push.NewSocket(); err != nil {
		log.WithFields(log.Fields{
			"type": "err_socket_new",
			"spec": pushSpec,
			"err":  err,
		}).Fatal("Socket new error")
	}
	if err = pushSock.Dial(pushSpec); err != nil {
		log.WithFields(log.Fields{
			"type": "err_socket_connect",
			"spec": pushSpec,
			"err":  err,
		}).Fatal("Socket connect error")
	}

	return pushSock
}

func enable(udid string) {
	ctx := gousb.NewContext()

	var usbDevice *gousb.Device = nil
	var activated bool
	if udid == "" {
		devs, err := findIosDevices(ctx)
		if err != nil {
			log.Errorf("Error finding iOS Devices - %s", err)
		}
		for _, dev := range devs {
			oneActivated := false
			oneUdid := ""
			if usbDevice == nil {
				oneUdid, _ = dev.SerialNumber()
				subcs := getVendorSubclasses(dev.Desc)
				for _, subc := range subcs {
					if int(subc) == 42 {
						oneActivated = true
					}
				}
				if oneActivated == false {
					usbDevice = dev
					udid = oneUdid
					activated = oneActivated
				} else {
					dev.Close()
				}
			} else {
				dev.Close()
			}
		}
		if udid != "" {
			log.Infof("Using first disabled device; uuid=%s", udid)
		}
	} else {
		usbDevice, activated = openDevice(ctx, udid)
		log.Info("Opened device")
	}

	if usbDevice == nil {
		log.Info("Could not find a disabled device to activate")
		ctx.Close()
		return
	}

	if activated == true {
		log.Info("Device already enabled")
		usbDevice.Close()
		ctx.Close()
		return
	}

	sendQTEnable(usbDevice)

	usbDevice.Close()
	ctx.Close()
}

func disable(udid string) {
	ctx := gousb.NewContext()

	var usbDevice *gousb.Device = nil
	var activated bool
	if udid == "" {
		devs, err := findIosDevices(ctx)
		if err != nil {
			log.Errorf("Error finding iOS Devices - %s", err)
		}
		for _, dev := range devs {
			oneActivated := true
			oneUdid := ""
			if usbDevice == nil {
				oneUdid, _ = dev.SerialNumber()
				subcs := getVendorSubclasses(dev.Desc)
				for _, subc := range subcs {
					if int(subc) == 42 {
						oneActivated = true
					}
				}
				if oneActivated == true {
					usbDevice = dev
					udid = oneUdid
					activated = oneActivated
				} else {
					dev.Close()
				}
			} else {
				dev.Close()
			}
		}
		if udid != "" {
			log.Infof("Using first enabled device; uuid=%s", udid)
		}
	} else {
		usbDevice, activated = openDevice(ctx, udid)

		log.Info("Opened device")
	}

	if usbDevice == nil {
		log.Info("Could not find a enabled device to disabled")
		ctx.Close()
		return
	}

	if activated == false {
		log.Info("Device already disabled")
		usbDevice.Close()
		ctx.Close()
		return
	}

	usbDevice.Reset()
	//sendQTDisable( usbDevice )

	usbDevice.Close()
	ctx.Close()
}

func startWithConsumer(consumer screencapture.CmSampleBufConsumer, udid string, stopChannel chan interface{}, stopChannel2 chan interface{}) bool {
	ctx := gousb.NewContext()

	var usbDevice *gousb.Device = nil
	var activated bool = false
	if udid == "" {
		devs, err := findIosDevices(ctx)
		if err != nil {
			log.Errorf("Error finding iOS Devices - %s", err)
		}
		for _, dev := range devs {
			if usbDevice == nil {
				udid, _ = dev.SerialNumber()
				subcs := getVendorSubclasses(dev.Desc)
				for _, subc := range subcs {
					if int(subc) == 42 {
						activated = true
					}
				}
				usbDevice = dev
			} else {
				dev.Close()
			}
		}
	} else {
		usbDevice, activated = openDevice(ctx, udid)
		log.Info("Opened device")
	}

	if !activated {
		log.Info("Not activated; attempting to activate")
		sendQTEnable(usbDevice)

		var i int = 0
		for {
			time.Sleep(500 * time.Millisecond)
			usbDevice.Close()
			var activated bool
			usbDevice, activated = openDevice(ctx, udid)
			if activated {
				break
			}
			i++
			if i > 5 {
				log.Debug("Failed activating config")
				return false
			}
		}
	}

	adapter := UsbAdapter{}

	mp := screencapture.NewMessageProcessor(&adapter, stopChannel, consumer, false)

	err := startReading(&adapter, usbDevice, &mp, stopChannel2)
	if err != nil {
		log.Errorf("startReading failure - %s", err)
		log.Info("Closing device")
		usbDevice.Close()
		ctx.Close()
		return false
	}

	log.Info("Closing device")
	usbDevice.Close()

	ctx.Close()

	return true
}

func sendQTEnable(device *gousb.Device) {
	val, err := device.Control(0x40, 0x52, 0x00, 0x02, []byte{})
	if err != nil {
		log.Warnf("Failed sending control transfer for enabling hidden QT config. Seems like this happens sometimes but it still works usually: %d, %s", val, err)
	}
	log.Debugf("Enabling QT config RC:%d", val)
}

// Based on  'screencapture.activator.sendQTDisableConfigControlRequest()'
func sendQTDisable(device *gousb.Device) {
	val, err := device.Control(0x40, 0x52, 0x00, 0x00, []byte{})
	if err != nil {
		log.Warnf("Failed sending control transfer for enabling hidden QT config. "+
			"Seems like this happens sometimes but it still works usually: $d, %s", val, err)
	}
	log.Debugf("Dsiabling QT config RC:%d", val)
}

func waitForSigInt(stopChannel chan interface{}, stopChannel2 chan interface{}, stopChannel3 chan bool) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		for sig := range c {
			fmt.Printf("Got signal %s\n", sig)
			go func() { stopChannel3 <- true }()
			go func() {
				stopChannel2 <- true
				stopChannel2 <- true
			}()
			go func() {
				stopChannel <- true
				stopChannel <- true
			}()

		}
	}()
}

// Stuff below more or less copied from quicktime_video_hack/screencapture/usbadapter.go and other files in that directory
// All of these stuff has to be copied in order to alter startReading due to non-exposed functions and variables

type UsbAdapter struct {
	outEndpoint *gousb.OutEndpoint
}

func (usa UsbAdapter) WriteDataToUsb(bytes []byte) {
	_, err := usa.outEndpoint.Write(bytes)
	if err != nil {
		log.Error("failed sending to usb", err)
	}
}

func startReading(usa *UsbAdapter, usbDevice *gousb.Device, receiver screencapture.UsbDataReceiver, stopSignal chan interface{}) error {
	var confignum int = 6

	config, err := usbDevice.Config(confignum)
	if err != nil {
		return errors.New("Could not retrieve config")
	}

	log.Debugf("QT Config is active: %s", config.String())

	/*val, err := usbDevice.Control(0x02, 0x01, 0, 0x86, make([]byte, 0))
	  if err != nil {
	      log.Debug("failed control", err)
	  }
	  log.Debugf("Clear Feature RC: %d", val)

	  val, err = usbDevice.Control(0x02, 0x01, 0, 0x05, make([]byte, 0))
	  if err != nil {
	      log.Debug("failed control", err)
	  }
	  log.Debugf("Clear Feature RC: %d", val)*/

	success, iface := findInterfaceForSubclass(config, 0x2a)
	if !success || iface == nil {
		log.Debug("could not get Quicktime Interface")
		return err
	}
	log.Debugf("Got QT iface:%s", iface.String())

	inboundBulkEndpointIndex, err := grabInBulk(iface.Setting)
	if err != nil {
		return err
	}
	inEndpoint, err := iface.InEndpoint(inboundBulkEndpointIndex)
	if err != nil {
		log.Error("couldnt get InEndpoint")
		return err
	}
	log.Debugf("Inbound Bulk: %s", inEndpoint.String())

	outboundBulkEndpointIndex, err := grabOutBulk(iface.Setting)
	if err != nil {
		log.Error("couldnt get OutEndpoint")
		return err
	}

	outEndpoint, err := iface.OutEndpoint(outboundBulkEndpointIndex)
	if err != nil {
		log.Error("couldnt get OutEndpoint")
		return err
	}
	log.Debugf("Outbound Bulk: %s", outEndpoint.String())
	usa.outEndpoint = outEndpoint

	stream, err := inEndpoint.NewStream(4096, 5)
	if err != nil {
		log.Fatal("couldnt create stream")
		return err
	}
	log.Debug("Endpoint claimed")
	udid, _ := usbDevice.SerialNumber()
	log.Infof("Device '%s' ready to stream ( click 'Settings-Developer-Reset Media Services' if nothing happens )", udid)

	go func() {
		for {
			buffer := make([]byte, 4)

			n, err := io.ReadFull(stream, buffer)
			if err != nil {
				log.Errorf("Failed reading 4bytes length with err:%s only received: %d", err, n)
				return
			}
			//the 4 bytes header are included in the length, so we need to subtract them
			//here to know how long the payload will be
			length := binary.LittleEndian.Uint32(buffer) - 4
			dataBuffer := make([]byte, length)

			n, err = io.ReadFull(stream, dataBuffer)
			if err != nil {
				log.Errorf("Failed reading payload with err:%s only received: %d/%d bytes", err, n, length)
				return
			}
			receiver.ReceiveData(dataBuffer)
		}
	}()

	<-stopSignal
	receiver.CloseSession()
	log.Info("Closing usb stream")

	err = stream.Close()
	if err != nil {
		log.Error("Error closing stream", err)
	}
	log.Info("Closing usb interface")
	iface.Close()

	log.Info("Closing config")
	config.Close()

	sendQTDisable(usbDevice)

	return nil
}

func grabOutBulk(setting gousb.InterfaceSetting) (int, error) {
	for _, v := range setting.Endpoints {
		if v.Direction == gousb.EndpointDirectionOut {
			return v.Number, nil
		}
	}
	return 0, errors.New("Outbound Bulkendpoint not found")
}

func grabInBulk(setting gousb.InterfaceSetting) (int, error) {
	for _, v := range setting.Endpoints {
		if v.Direction == gousb.EndpointDirectionIn {
			return v.Number, nil
		}
	}
	return 0, errors.New("Inbound Bulkendpoint not found")
}

func findInterfaceForSubclass(config *gousb.Config, subClass gousb.Class) (bool, *gousb.Interface) {
	for _, ifaced := range config.Desc.Interfaces {
		if ifaced.AltSettings[0].Class == gousb.ClassVendorSpec &&
			ifaced.AltSettings[0].SubClass == subClass {
			iface, _ := config.Interface(ifaced.Number, 0)
			return true, iface
		}
	}
	return false, nil
}
