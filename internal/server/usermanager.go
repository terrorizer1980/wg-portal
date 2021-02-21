package server

import (
	"bytes"
	"crypto/md5"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"
	"github.com/h44z/wg-portal/internal/common"
	"github.com/h44z/wg-portal/internal/ldap"
	"github.com/h44z/wg-portal/internal/wireguard"
	"github.com/sirupsen/logrus"
	"github.com/skip2/go-qrcode"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

//
// CUSTOM VALIDATORS ----------------------------------------------------------------------------
//
var cidrList validator.Func = func(fl validator.FieldLevel) bool {
	cidrListStr := fl.Field().String()

	cidrList := common.ParseStringList(cidrListStr)
	for i := range cidrList {
		_, _, err := net.ParseCIDR(cidrList[i])
		if err != nil {
			return false
		}
	}
	return true
}

var ipList validator.Func = func(fl validator.FieldLevel) bool {
	ipListStr := fl.Field().String()

	ipList := common.ParseStringList(ipListStr)
	for i := range ipList {
		ip := net.ParseIP(ipList[i])
		if ip == nil {
			return false
		}
	}
	return true
}

func init() {
	if v, ok := binding.Validator.Engine().(*validator.Validate); ok {
		_ = v.RegisterValidation("cidrlist", cidrList)
		_ = v.RegisterValidation("iplist", ipList)
	}
}

//
//  PEER ----------------------------------------------------------------------------------------
//

type Peer struct {
	Peer     *wgtypes.Peer              `gorm:"-"`
	LdapUser *ldap.UserCacheHolderEntry `gorm:"-"` // optional, it is still possible to have users without ldap
	Config   string                     `gorm:"-"`

	UID               string `form:"uid" binding:"alphanum"` // uid for html identification
	IsOnline          bool   `gorm:"-"`
	IsNew             bool   `gorm:"-"`
	Identifier        string `form:"identifier" binding:"required,lt=64"` // Identifier AND Email make a WireGuard peer unique
	Email             string `gorm:"index" form:"mail" binding:"required,email"`
	LastHandshake     string `gorm:"-"`
	LastHandshakeTime string `gorm:"-"`

	IgnorePersistentKeepalive bool     `form:"ignorekeepalive"`
	PresharedKey              string   `form:"presharedkey" binding:"omitempty,base64"`
	AllowedIPsStr             string   `form:"allowedip" binding:"cidrlist"`
	IPsStr                    string   `form:"ip" binding:"cidrlist"`
	AllowedIPs                []string `gorm:"-"` // IPs that are used in the client config file
	IPs                       []string `gorm:"-"` // The IPs of the client
	PrivateKey                string   `form:"privkey" binding:"omitempty,base64"`
	PublicKey                 string   `gorm:"primaryKey" form:"pubkey" binding:"required,base64"`

	DeactivatedAt *time.Time
	CreatedBy     string
	UpdatedBy     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (p Peer) GetConfig() wgtypes.PeerConfig {
	publicKey, _ := wgtypes.ParseKey(p.PublicKey)
	var presharedKey *wgtypes.Key
	if p.PresharedKey != "" {
		presharedKeyTmp, _ := wgtypes.ParseKey(p.PresharedKey)
		presharedKey = &presharedKeyTmp
	}

	cfg := wgtypes.PeerConfig{
		PublicKey:                   publicKey,
		Remove:                      false,
		UpdateOnly:                  false,
		PresharedKey:                presharedKey,
		Endpoint:                    nil,
		PersistentKeepaliveInterval: nil,
		ReplaceAllowedIPs:           true,
		AllowedIPs:                  make([]net.IPNet, len(p.IPs)),
	}
	for i, ip := range p.IPs {
		_, ipNet, err := net.ParseCIDR(ip)
		if err == nil {
			cfg.AllowedIPs[i] = *ipNet
		}
	}

	return cfg
}

func (p Peer) GetConfigFile(device Device) ([]byte, error) {
	tpl, err := template.New("client").Funcs(template.FuncMap{"StringsJoin": strings.Join}).Parse(wireguard.ClientCfgTpl)
	if err != nil {
		return nil, err
	}

	var tplBuff bytes.Buffer

	err = tpl.Execute(&tplBuff, struct {
		Client Peer
		Server Device
	}{
		Client: p,
		Server: device,
	})
	if err != nil {
		return nil, err
	}

	return tplBuff.Bytes(), nil
}

func (p Peer) GetQRCode() ([]byte, error) {
	png, err := qrcode.Encode(p.Config, qrcode.Medium, 250)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"err": err,
		}).Error("failed to create qrcode")
		return nil, err
	}
	return png, nil
}

func (p Peer) IsValid() bool {
	if p.PublicKey == "" {
		return false
	}

	return true
}

func (p Peer) ToMap() map[string]string {
	out := make(map[string]string)

	v := reflect.ValueOf(p)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}

	typ := v.Type()
	for i := 0; i < v.NumField(); i++ {
		// gets us a StructField
		fi := typ.Field(i)
		if tagv := fi.Tag.Get("form"); tagv != "" {
			// set key of map to value in struct field
			out[tagv] = v.Field(i).String()
		}
	}
	return out
}

func (p Peer) GetConfigFileName() string {
	reg := regexp.MustCompile("[^a-zA-Z0-9_-]+")
	return reg.ReplaceAllString(strings.ReplaceAll(p.Identifier, " ", "-"), "") + ".conf"
}

//
//  DEVICE --------------------------------------------------------------------------------------
//

type Device struct {
	Interface *wgtypes.Device `gorm:"-"`

	DeviceName          string   `form:"device" gorm:"primaryKey" binding:"required,alphanum"`
	PrivateKey          string   `form:"privkey" binding:"required,base64"`
	PublicKey           string   `form:"pubkey" binding:"required,base64"`
	PersistentKeepalive int      `form:"keepalive" binding:"gte=0"`
	ListenPort          int      `form:"port" binding:"required,gt=0"`
	Mtu                 int      `form:"mtu" binding:"gte=0,lte=1500"`
	Endpoint            string   `form:"endpoint" binding:"required,hostname_port"`
	AllowedIPsStr       string   `form:"allowedip" binding:"cidrlist"`
	IPsStr              string   `form:"ip" binding:"required,cidrlist"`
	AllowedIPs          []string `gorm:"-"` // IPs that are used in the client config file
	IPs                 []string `gorm:"-"` // The IPs of the client
	DNSStr              string   `form:"dns" binding:"iplist"`
	DNS                 []string `gorm:"-"` // The DNS servers of the client
	PreUp               string   `form:"preup"`
	PostUp              string   `form:"postup"`
	PreDown             string   `form:"predown"`
	PostDown            string   `form:"postdown"`
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

func (d Device) IsValid() bool {
	if d.PublicKey == "" {
		return false
	}
	if len(d.IPs) == 0 {
		return false
	}
	if d.Endpoint == "" {
		return false
	}

	return true
}

func (d Device) GetConfig() wgtypes.Config {
	var privateKey *wgtypes.Key
	if d.PrivateKey != "" {
		pKey, _ := wgtypes.ParseKey(d.PrivateKey)
		privateKey = &pKey
	}

	cfg := wgtypes.Config{
		PrivateKey: privateKey,
		ListenPort: &d.ListenPort,
	}

	return cfg
}

func (d Device) GetConfigFile(clients []Peer) ([]byte, error) {
	tpl, err := template.New("server").Funcs(template.FuncMap{"StringsJoin": strings.Join}).Parse(wireguard.DeviceCfgTpl)
	if err != nil {
		return nil, err
	}

	var tplBuff bytes.Buffer

	err = tpl.Execute(&tplBuff, struct {
		Clients []Peer
		Server  Device
	}{
		Clients: clients,
		Server:  d,
	})
	if err != nil {
		return nil, err
	}

	return tplBuff.Bytes(), nil
}

//
//  USER-MANAGER --------------------------------------------------------------------------------
//

type UserManager struct {
	db        *gorm.DB
	wg        *wireguard.Manager
	ldapUsers *ldap.SynchronizedUserCacheHolder
}

func NewUserManager(dbPath string, wg *wireguard.Manager, ldapUsers *ldap.SynchronizedUserCacheHolder) *UserManager {

	um := &UserManager{wg: wg, ldapUsers: ldapUsers}
	var err error
	if _, err = os.Stat(filepath.Dir(dbPath)); os.IsNotExist(err) {
		if err = os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
			logrus.Errorf("failed to create database directory (%s): %v", filepath.Dir(dbPath), err)
			return nil
		}
	}
	um.db, err = gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		logrus.Errorf("failed to open sqlite database (%s): %v", dbPath, err)
		return nil
	}

	err = um.db.AutoMigrate(&Peer{}, &Device{})
	if err != nil {
		logrus.Errorf("failed to migrate sqlite database: %v", err)
		return nil
	}

	return um
}

func (u *UserManager) InitFromCurrentInterface() error {
	peers, err := u.wg.GetPeerList()
	if err != nil {
		logrus.Errorf("failed to init user-manager from peers: %v", err)
		return err
	}
	device, err := u.wg.GetDeviceInfo()
	if err != nil {
		logrus.Errorf("failed to init user-manager from device: %v", err)
		return err
	}
	var ipAddresses []string
	var mtu int
	if u.wg.Cfg.ManageIPAddresses {
		if ipAddresses, err = u.wg.GetIPAddress(); err != nil {
			logrus.Errorf("failed to init user-manager from device: %v", err)
			return err
		}
		if mtu, err = u.wg.GetMTU(); err != nil {
			logrus.Errorf("failed to init user-manager from device: %v", err)
			return err
		}
	}

	// Check if entries already exist in database, if not create them
	for _, peer := range peers {
		if err := u.validateOrCreateUserForPeer(peer); err != nil {
			return err
		}
	}
	if err := u.validateOrCreateDevice(*device, ipAddresses, mtu); err != nil {
		return err
	}

	return nil
}

func (u *UserManager) validateOrCreateUserForPeer(wgPeer wgtypes.Peer) error {
	peer := Peer{}
	u.db.Where("public_key = ?", wgPeer.PublicKey.String()).FirstOrInit(&peer)

	if peer.PublicKey == "" { // peer not found, create
		peer.UID = fmt.Sprintf("u%x", md5.Sum([]byte(wgPeer.PublicKey.String())))
		peer.PublicKey = wgPeer.PublicKey.String()
		peer.PrivateKey = "" // UNKNOWN
		if wgPeer.PresharedKey != (wgtypes.Key{}) {
			peer.PresharedKey = wgPeer.PresharedKey.String()
		}
		peer.Email = "autodetected@example.com"
		peer.Identifier = "Autodetected (" + peer.PublicKey[0:8] + ")"
		peer.UpdatedAt = time.Now()
		peer.CreatedAt = time.Now()
		peer.AllowedIPs = make([]string, 0) // UNKNOWN
		peer.IPs = make([]string, len(wgPeer.AllowedIPs))
		for i, ip := range wgPeer.AllowedIPs {
			peer.IPs[i] = ip.String()
		}
		peer.AllowedIPsStr = strings.Join(peer.AllowedIPs, ", ")
		peer.IPsStr = strings.Join(peer.IPs, ", ")

		res := u.db.Create(&peer)
		if res.Error != nil {
			logrus.Errorf("failed to create autodetected wgPeer: %v", res.Error)
			return res.Error
		}
	}

	return nil
}

func (u *UserManager) validateOrCreateDevice(dev wgtypes.Device, ipAddresses []string, mtu int) error {
	device := Device{}
	u.db.Where("device_name = ?", dev.Name).FirstOrInit(&device)

	if device.PublicKey == "" { // device not found, create
		device.PublicKey = dev.PublicKey.String()
		device.PrivateKey = dev.PrivateKey.String()
		device.DeviceName = dev.Name
		device.ListenPort = dev.ListenPort
		device.Mtu = 0
		device.PersistentKeepalive = 16 // Default
		device.IPsStr = strings.Join(ipAddresses, ", ")
		if mtu == wireguard.DefaultMTU {
			mtu = 0
		}
		device.Mtu = mtu

		res := u.db.Create(&device)
		if res.Error != nil {
			logrus.Errorf("failed to create autodetected device: %v", res.Error)
			return res.Error
		}
	}

	return nil
}

func (u *UserManager) populateUserData(user *Peer) {
	user.AllowedIPs = strings.Split(user.AllowedIPsStr, ", ")
	user.IPs = strings.Split(user.IPsStr, ", ")
	// Set config file
	tmpCfg, _ := user.GetConfigFile(u.GetDevice())
	user.Config = string(tmpCfg)

	// set data from WireGuard interface
	user.Peer, _ = u.wg.GetPeer(user.PublicKey)
	user.LastHandshake = "never"
	user.LastHandshakeTime = "Never connected, or user is disabled."
	if user.Peer != nil {
		since := time.Since(user.Peer.LastHandshakeTime)
		sinceSeconds := int(since.Round(time.Second).Seconds())
		sinceMinutes := int(sinceSeconds / 60)
		sinceSeconds -= sinceMinutes * 60

		if sinceMinutes > 2*10080 { // 2 weeks
			user.LastHandshake = "a while ago"
		} else if sinceMinutes > 10080 { // 1 week
			user.LastHandshake = "a week ago"
		} else {
			user.LastHandshake = fmt.Sprintf("%02dm %02ds", sinceMinutes, sinceSeconds)
		}
		user.LastHandshakeTime = user.Peer.LastHandshakeTime.Format(time.UnixDate)
	}
	user.IsOnline = false // todo: calculate online status

	// set ldap data
	user.LdapUser = u.ldapUsers.GetUserData(u.ldapUsers.GetUserDNByMail(user.Email))
}

func (u *UserManager) populateDeviceData(device *Device) {
	device.AllowedIPs = strings.Split(device.AllowedIPsStr, ", ")
	device.IPs = strings.Split(device.IPsStr, ", ")
	device.DNS = strings.Split(device.DNSStr, ", ")

	// set data from WireGuard interface
	device.Interface, _ = u.wg.GetDeviceInfo()
}

func (u *UserManager) GetAllUsers() []Peer {
	peers := make([]Peer, 0)
	u.db.Find(&peers)

	for i := range peers {
		u.populateUserData(&peers[i])
	}

	return peers
}

func (u *UserManager) GetActiveUsers() []Peer {
	peers := make([]Peer, 0)
	u.db.Where("deactivated_at IS NULL").Find(&peers)

	for i := range peers {
		u.populateUserData(&peers[i])
	}

	return peers
}

func (u *UserManager) GetFilteredAndSortedUsers(sortKey, sortDirection, search string) []Peer {
	peers := make([]Peer, 0)
	u.db.Find(&peers)

	filteredPeers := make([]Peer, 0, len(peers))
	for i := range peers {
		u.populateUserData(&peers[i])

		if search == "" ||
			strings.Contains(peers[i].Email, search) ||
			strings.Contains(peers[i].Identifier, search) ||
			strings.Contains(peers[i].PublicKey, search) {
			filteredPeers = append(filteredPeers, peers[i])
		}
	}

	sort.Slice(filteredPeers, func(i, j int) bool {
		var sortValueLeft string
		var sortValueRight string

		switch sortKey {
		case "id":
			sortValueLeft = filteredPeers[i].Identifier
			sortValueRight = filteredPeers[j].Identifier
		case "pubKey":
			sortValueLeft = filteredPeers[i].PublicKey
			sortValueRight = filteredPeers[j].PublicKey
		case "mail":
			sortValueLeft = filteredPeers[i].Email
			sortValueRight = filteredPeers[j].Email
		case "ip":
			sortValueLeft = filteredPeers[i].IPsStr
			sortValueRight = filteredPeers[j].IPsStr
		case "handshake":
			if filteredPeers[i].Peer == nil {
				return false
			} else if filteredPeers[j].Peer == nil {
				return true
			}
			sortValueLeft = filteredPeers[i].Peer.LastHandshakeTime.Format(time.RFC3339)
			sortValueRight = filteredPeers[j].Peer.LastHandshakeTime.Format(time.RFC3339)
		}

		if sortDirection == "asc" {
			return sortValueLeft < sortValueRight
		} else {
			return sortValueLeft > sortValueRight
		}
	})

	return filteredPeers
}

func (u *UserManager) GetSortedUsersForEmail(sortKey, sortDirection, email string) []Peer {
	peers := make([]Peer, 0)
	u.db.Where("email = ?", email).Find(&peers)

	for i := range peers {
		u.populateUserData(&peers[i])
	}

	sort.Slice(peers, func(i, j int) bool {
		var sortValueLeft string
		var sortValueRight string

		switch sortKey {
		case "id":
			sortValueLeft = peers[i].Identifier
			sortValueRight = peers[j].Identifier
		case "pubKey":
			sortValueLeft = peers[i].PublicKey
			sortValueRight = peers[j].PublicKey
		case "mail":
			sortValueLeft = peers[i].Email
			sortValueRight = peers[j].Email
		case "ip":
			sortValueLeft = peers[i].IPsStr
			sortValueRight = peers[j].IPsStr
		case "handshake":
			if peers[i].Peer == nil {
				return true
			} else if peers[j].Peer == nil {
				return false
			}
			sortValueLeft = peers[i].Peer.LastHandshakeTime.Format(time.RFC3339)
			sortValueRight = peers[j].Peer.LastHandshakeTime.Format(time.RFC3339)
		}

		if sortDirection == "asc" {
			return sortValueLeft < sortValueRight
		} else {
			return sortValueLeft > sortValueRight
		}
	})

	return peers
}

func (u *UserManager) GetDevice() Device {
	devices := make([]Device, 0, 1)
	u.db.Find(&devices)

	for i := range devices {
		u.populateDeviceData(&devices[i])
	}

	return devices[0] // use first device for now... more to come?
}

func (u *UserManager) GetUserByKey(publicKey string) Peer {
	peer := Peer{}
	u.db.Where("public_key = ?", publicKey).FirstOrInit(&peer)
	u.populateUserData(&peer)
	return peer
}

func (u *UserManager) GetUsersByMail(mail string) []Peer {
	var peers []Peer
	u.db.Where("email = ?", mail).Find(&peers)
	for i := range peers {
		u.populateUserData(&peers[i])
	}

	return peers
}

func (u *UserManager) CreateUser(peer Peer) error {
	peer.UID = fmt.Sprintf("u%x", md5.Sum([]byte(peer.PublicKey)))
	peer.UpdatedAt = time.Now()
	peer.CreatedAt = time.Now()
	peer.AllowedIPsStr = strings.Join(peer.AllowedIPs, ", ")
	peer.IPsStr = strings.Join(peer.IPs, ", ")

	res := u.db.Create(&peer)
	if res.Error != nil {
		logrus.Errorf("failed to create peer: %v", res.Error)
		return res.Error
	}

	return nil
}

func (u *UserManager) UpdateUser(peer Peer) error {
	peer.UpdatedAt = time.Now()
	peer.AllowedIPsStr = strings.Join(peer.AllowedIPs, ", ")
	peer.IPsStr = strings.Join(peer.IPs, ", ")

	res := u.db.Save(&peer)
	if res.Error != nil {
		logrus.Errorf("failed to update peer: %v", res.Error)
		return res.Error
	}

	return nil
}

func (u *UserManager) DeleteUser(peer Peer) error {
	res := u.db.Delete(&peer)
	if res.Error != nil {
		logrus.Errorf("failed to delete peer: %v", res.Error)
		return res.Error
	}

	return nil
}

func (u *UserManager) UpdateDevice(device Device) error {
	device.UpdatedAt = time.Now()
	device.AllowedIPsStr = strings.Join(device.AllowedIPs, ", ")
	device.IPsStr = strings.Join(device.IPs, ", ")
	device.DNSStr = strings.Join(device.DNS, ", ")

	res := u.db.Save(&device)
	if res.Error != nil {
		logrus.Errorf("failed to update device: %v", res.Error)
		return res.Error
	}

	return nil
}

func (u *UserManager) GetAllReservedIps() ([]string, error) {
	reservedIps := make([]string, 0)
	users := u.GetAllUsers()
	for _, user := range users {
		for _, cidr := range user.IPs {
			if cidr == "" {
				continue
			}
			ip, _, err := net.ParseCIDR(cidr)
			if err != nil {
				return nil, err
			}
			reservedIps = append(reservedIps, ip.String())
		}
	}

	device := u.GetDevice()
	for _, cidr := range device.IPs {
		if cidr == "" {
			continue
		}
		ip, _, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, err
		}

		reservedIps = append(reservedIps, ip.String())
	}

	return reservedIps, nil
}

func (u *UserManager) IsIPReserved(cidr string) bool {
	reserved, err := u.GetAllReservedIps()
	if err != nil {
		return true // in case something failed, assume the ip is reserved
	}
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return true
	}

	// this two addresses are not usable
	broadcastAddr := common.BroadcastAddr(ipnet).String()
	networkAddr := ipnet.IP.String()
	address := ip.String()

	if address == broadcastAddr || address == networkAddr {
		return true
	}

	for _, r := range reserved {
		if address == r {
			return true
		}
	}

	return false
}

// GetAvailableIp search for an available ip in cidr against a list of reserved ips
func (u *UserManager) GetAvailableIp(cidr string) (string, error) {
	reserved, err := u.GetAllReservedIps()
	if err != nil {
		return "", err
	}
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", err
	}

	// this two addresses are not usable
	broadcastAddr := common.BroadcastAddr(ipnet).String()
	networkAddr := ipnet.IP.String()

	for ip := ip.Mask(ipnet.Mask); ipnet.Contains(ip); common.IncreaseIP(ip) {
		ok := true
		address := ip.String()
		for _, r := range reserved {
			if address == r {
				ok = false
				break
			}
		}
		if ok && address != networkAddr && address != broadcastAddr {
			netMask := "/32"
			if common.IsIPv6(address) {
				netMask = "/128"
			}
			return address + netMask, nil
		}
	}

	return "", errors.New("no more available address from cidr")
}
