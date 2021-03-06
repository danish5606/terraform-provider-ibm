package ibm

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform/helper/resource"
	"github.com/hashicorp/terraform/helper/schema"
	"github.com/softlayer/softlayer-go/datatypes"
	"github.com/softlayer/softlayer-go/filter"
	"github.com/softlayer/softlayer-go/helpers/product"
	"github.com/softlayer/softlayer-go/helpers/virtual"
	"github.com/softlayer/softlayer-go/services"
	"github.com/softlayer/softlayer-go/session"
	"github.com/softlayer/softlayer-go/sl"
)

type storageCustomIds []int

func (s storageCustomIds) Storages(meta interface{}) ([]datatypes.Network_Storage, error) {
	storageService := services.GetNetworkStorageService(meta.(ClientSession).SoftLayerSession())
	storages := make([]datatypes.Network_Storage, len(s))

	for i, id := range s {
		var err error
		storages[i], err = storageService.Id(id).GetObject()
		if err != nil {
			return nil, err
		}
	}
	return storages, nil
}

const (
	staticIPRoutedCustom = "STATIC_IP_ROUTED"

	upgradeTransactionCustom = "UPGRADE"
	pendingUpgradeCustom     = "pending_upgrade"
	inProgressUpgradeCustom  = "upgrade_started"
	idleTransactionCustom    = "idle"

	virtualGuestAvailableCustom    = "available"
	virtualGuestProvisioningCustom = "provisioning"

	networkStorageMassAccessControlModificationExceptionCustom = "SoftLayer_Exception_Network_Storage_Group_MassAccessControlModification"
	retryDelayForModifyingStorageAccessCustom                  = 10 * time.Second
)

func resourceIBMComputeCustomVmInstance() *schema.Resource {
	return &schema.Resource{
		Create:   resourceIBMComputeCustomVmInstanceCreate,
		Read:     resourceIBMComputeCustomVmInstanceRead,
		Update:   resourceIBMComputeCustomVmInstanceUpdate,
		Delete:   resourceIBMComputeCustomVmInstanceDelete,
		Exists:   resourceIBMComputeCustomVmInstanceExists,
		Importer: &schema.ResourceImporter{},

		Schema: map[string]*schema.Schema{
			"hostname": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: genIDCustom,
				DiffSuppressFunc: func(k, o, n string, d *schema.ResourceData) bool {
					// FIXME: Work around another bug in terraform.
					// When a default function is used with an optional property,
					// terraform will always execute it on apply, even when the property
					// already has a value in the state for it. This causes a false diff.
					// Making the property Computed:true does not make a difference.
					if strings.HasPrefix(o, "terraformed-") && strings.HasPrefix(n, "terraformed-") {
						return true
					}
					return o == n
				},
			},

			"domain": {
				Type:     schema.TypeString,
				Required: true,
			},

			"os_reference_code": {
				Type:          schema.TypeString,
				Optional:      true,
				ForceNew:      true,
				ConflictsWith: []string{"image_id"},
			},

			"hourly_billing": {
				Type:     schema.TypeBool,
				Optional: true,
				Default:  true,
				ForceNew: true,
			},

			"private_network_only": {
				Type:     schema.TypeBool,
				Optional: true,
				Default:  false,
				ForceNew: true,
			},

			"datacenter": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},

			"flavor_key_name": {
				Type:          schema.TypeString,
				Optional:      true,
				Description:   "Flavor key name used to provision vm.",
				ConflictsWith: []string{"cores", "memory"},
			},

			"cores": {

				Type:          schema.TypeInt,
				Optional:      true,
				Computed:      true,
				ConflictsWith: []string{"flavor_key_name"},
			},

			"memory": {
				Type:     schema.TypeInt,
				Optional: true,
				Computed: true,
				ValidateFunc: func(v interface{}, k string) (ws []string, errors []error) {
					memoryInMB := float64(v.(int))

					// Validate memory to match gigs format
					remaining := math.Mod(memoryInMB, 1024)
					if remaining > 0 {
						suggested := math.Ceil(memoryInMB/1024) * 1024
						errors = append(errors, fmt.Errorf(
							"Invalid 'memory' value %d megabytes, must be a multiple of 1024 (e.g. use %d)", int(memoryInMB), int(suggested)))
					}

					return
				},
				ConflictsWith: []string{"flavor_key_name"},
			},

			"dedicated_acct_host_only": {
				Type:          schema.TypeBool,
				Optional:      true,
				ForceNew:      true,
				ConflictsWith: []string{"dedicated_host_name", "dedicated_host_id"},
			},

			"dedicated_host_name": {
				Type:          schema.TypeString,
				Optional:      true,
				ForceNew:      true,
				ConflictsWith: []string{"dedicated_acct_host_only", "dedicated_host_id"},
				DiffSuppressFunc: func(k, old, new string, d *schema.ResourceData) bool {
					_, ok := d.GetOk("dedicated_host_id")
					return new == "" && ok
				},
			},

			"dedicated_host_id": {
				Type:          schema.TypeInt,
				Optional:      true,
				ForceNew:      true,
				ConflictsWith: []string{"dedicated_acct_host_only", "dedicated_host_name"},
				DiffSuppressFunc: func(k, old, new string, d *schema.ResourceData) bool {
					_, ok := d.GetOk("dedicated_host_name")
					return new == "0" && ok
				},
			},

			"public_vlan_id": {
				Type:     schema.TypeInt,
				Optional: true,
				ForceNew: true,
				Computed: true,
			},
			"public_interface_id": {
				Type:     schema.TypeInt,
				Computed: true,
			},
			"public_subnet": {
				Type:     schema.TypeString,
				Optional: true,
				ForceNew: true,
				Computed: true,
			},
			"secret": {
				Type:     schema.TypeString,
				Optional: true,
				Computed: true,
			},
			"public_subnet_id": {
				Type:     schema.TypeInt,
				Computed: true,
			},

			"public_security_group_ids": {
				Type:     schema.TypeSet,
				Optional: true,
				Computed: true,
				Elem:     &schema.Schema{Type: schema.TypeInt},
				Set: func(v interface{}) int {
					return v.(int)
				},
				ForceNew: true,
				MaxItems: 5,
			},

			"private_vlan_id": {
				Type:     schema.TypeInt,
				Optional: true,
				ForceNew: true,
				Computed: true,
			},
			"private_interface_id": {
				Type:     schema.TypeInt,
				Computed: true,
			},
			"private_subnet": {
				Type:     schema.TypeString,
				Optional: true,
				ForceNew: true,
				Computed: true,
			},

			"private_subnet_id": {
				Type:     schema.TypeInt,
				Computed: true,
			},

			"private_security_group_ids": {
				Type:     schema.TypeSet,
				Optional: true,
				Computed: true,
				Elem:     &schema.Schema{Type: schema.TypeInt},
				Set: func(v interface{}) int {
					return v.(int)
				},
				ForceNew: true,
				MaxItems: 5,
			},

			"disks": {
				Type:     schema.TypeList,
				Optional: true,
				Computed: true,
				Elem:     &schema.Schema{Type: schema.TypeInt},
			},

			"network_speed": {
				Type:     schema.TypeInt,
				Optional: true,
				Default:  100,
			},

			"ipv4_address": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"ipv4_address_private": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"ip_address_id": {
				Type:     schema.TypeInt,
				Computed: true,
			},

			"ip_address_id_private": {
				Type:     schema.TypeInt,
				Computed: true,
			},

			"ipv6_enabled": {
				Type:     schema.TypeBool,
				Optional: true,
				ForceNew: true,
				Default:  false,
			},

			"ipv6_static_enabled": {
				Type:     schema.TypeBool,
				Optional: true,
				ForceNew: true,
				Default:  false,
			},

			"ipv6_address": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"ipv6_address_id": {
				Type:     schema.TypeInt,
				Computed: true,
			},

			// SoftLayer does not support public_ipv6_subnet configuration in vm creation. So, public_ipv6_subnet
			// is defined as a computed parameter.
			"public_ipv6_subnet": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"public_ipv6_subnet_id": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"secondary_ip_count": {
				Type:         schema.TypeInt,
				Optional:     true,
				ForceNew:     true,
				ValidateFunc: validateSecondaryIPCount,
				DiffSuppressFunc: func(k, o, n string, d *schema.ResourceData) bool {
					// secondary_ip_count is only used when a virtual_guest resource is created.
					if d.State() == nil {
						return false
					}
					return true
				},
			},

			"secondary_ip_addresses": {
				Type:     schema.TypeList,
				Computed: true,
				Elem:     &schema.Schema{Type: schema.TypeString},
			},

			"ssh_key_ids": {
				Type:     schema.TypeSet,
				Optional: true,
				Elem:     &schema.Schema{Type: schema.TypeInt},
				Set: func(v interface{}) int {
					return v.(int)
				},
			},

			"file_storage_ids": {
				Type:     schema.TypeSet,
				Optional: true,
				Computed: true,
				Elem:     &schema.Schema{Type: schema.TypeInt},
				Set: func(v interface{}) int {
					return v.(int)
				},
			},

			"block_storage_ids": {
				Type:     schema.TypeSet,
				Optional: true,
				Computed: true,
				Elem:     &schema.Schema{Type: schema.TypeInt},
				Set: func(v interface{}) int {
					return v.(int)
				},
			},
			"user_metadata": {
				Type:     schema.TypeString,
				Optional: true,
				ForceNew: true,
			},

			"notes": {
				Type:         schema.TypeString,
				Optional:     true,
				ValidateFunc: validateNotes,
			},

			"local_disk": {
				Type:     schema.TypeBool,
				Optional: true,
				Default:  true,
				ForceNew: true,
			},

			"post_install_script_uri": {
				Type:     schema.TypeString,
				Optional: true,
				Default:  nil,
				ForceNew: true,
			},

			"image_id": {
				Type:          schema.TypeInt,
				Optional:      true,
				ForceNew:      true,
				ConflictsWith: []string{"os_reference_code"},
			},

			"tags": {
				Type:     schema.TypeSet,
				Optional: true,
				Elem:     &schema.Schema{Type: schema.TypeString},
				Set:      schema.HashString,
			},
			//waiting time modified to 24 hours
			"wait_time_minutes": {
				Type:     schema.TypeInt,
				Optional: true,
				Default:  1440,
			},
			// Monthly only
			// Limited BandWidth
			"public_bandwidth_limited": {
				Type:             schema.TypeInt,
				Optional:         true,
				Computed:         true,
				ForceNew:         true,
				DiffSuppressFunc: applyOnce,
				ConflictsWith:    []string{"private_network_only", "public_bandwidth_unlimited"},
				ValidateFunc:     validatePublicBandwidth,
			},

			// Monthly only
			// Unlimited BandWidth
			"public_bandwidth_unlimited": {
				Type:             schema.TypeBool,
				Optional:         true,
				Default:          false,
				ForceNew:         true,
				DiffSuppressFunc: applyOnce,
				ConflictsWith:    []string{"private_network_only", "public_bandwidth_limited"},
			},
		},
	}
}

func getSubnetIDCustom(subnet string, meta interface{}) (int, error) {
	service := services.GetAccountService(meta.(ClientSession).SoftLayerSession())

	subnetInfo := strings.Split(subnet, "/")
	if len(subnetInfo) != 2 {
		return 0, fmt.Errorf(
			"Unable to parse the provided subnet: %s", subnet)
	}

	networkIdentifier := subnetInfo[0]
	cidr := subnetInfo[1]

	subnets, err := service.
		Mask("id").
		Filter(
			filter.Build(
				filter.Path("subnets.cidr").Eq(cidr),
				filter.Path("subnets.networkIdentifier").Eq(networkIdentifier),
			),
		).
		GetSubnets()

	if err != nil {
		return 0, fmt.Errorf("Error looking up Subnet: %s", err)
	}

	if len(subnets) < 1 {
		return 0, fmt.Errorf(
			"Unable to locate a subnet matching the provided subnet: %s", subnet)
	}

	return *subnets[0].Id, nil
}

func getNameForBlockDeviceCustom(i int) string {
	// skip 1, which is reserved for the swap disk.
	// so we get 0, 2, 3, 4, 5 ...
	if i == 0 {
		return "0"
	}

	return strconv.Itoa(i + 1)
}

func getNameForBlockDeviceWithFlavorCustom(i int) string {
	// skip 0, which is taken from flavor.
	// skip 1, which is reserved for the swap disk.
	// so we get  2, 3, 4, 5 ...

	return strconv.Itoa(i + 2)
}

func getBlockDevicesCustom(d *schema.ResourceData) []datatypes.Virtual_Guest_Block_Device {
	numBlocks := d.Get("disks.#").(int)
	if numBlocks == 0 {
		return nil
	}
	blocks := make([]datatypes.Virtual_Guest_Block_Device, 0, numBlocks)
	for i := 0; i < numBlocks; i++ {
		var name string
		blockRef := fmt.Sprintf("disks.%d", i)
		if _, ok := d.GetOk("flavor_key_name"); ok {
			name = getNameForBlockDeviceWithFlavorCustom(i)
		} else {
			name = getNameForBlockDeviceCustom(i)
		}
		capacity := d.Get(blockRef).(int)
		block := datatypes.Virtual_Guest_Block_Device{
			Device: &name,
			DiskImage: &datatypes.Virtual_Disk_Image{
				Capacity: &capacity,
			},
		}
		blocks = append(blocks, block)
	}

	return blocks
}

func expandSecurityGroupBindingsCustom(securityGroupsList []interface{}) ([]datatypes.Virtual_Network_SecurityGroup_NetworkComponentBinding, error) {
	if len(securityGroupsList) == 0 {
		return nil, nil
	}
	sgBindings := make([]datatypes.Virtual_Network_SecurityGroup_NetworkComponentBinding,
		len(securityGroupsList))
	for i, v := range securityGroupsList {
		sgid := v.(int)
		sgBindings[i] = datatypes.Virtual_Network_SecurityGroup_NetworkComponentBinding{
			SecurityGroupId: sl.Int(sgid),
		}
	}
	return sgBindings, nil
}

func getVirtualGuestTemplateFromResourceDataCustom(d *schema.ResourceData, meta interface{}) (datatypes.Virtual_Guest, error) {

	dc := datatypes.Location{
		Name: sl.String(d.Get("datacenter").(string)),
	}
	// FIXME: Work around bug in terraform (?)
	// For properties that have a default value set and a diff suppress function,
	// it is not using the default value.
	networkSpeed := d.Get("network_speed").(int)
	if networkSpeed == 0 {
		networkSpeed = resourceIBMComputeCustomVmInstance().Schema["network_speed"].Default.(int)
	}

	networkComponent := datatypes.Virtual_Guest_Network_Component{
		MaxSpeed: &networkSpeed,
	}

	opts := datatypes.Virtual_Guest{
		Hostname:               sl.String(d.Get("hostname").(string)),
		Domain:                 sl.String(d.Get("domain").(string)),
		HourlyBillingFlag:      sl.Bool(d.Get("hourly_billing").(bool)),
		PrivateNetworkOnlyFlag: sl.Bool(d.Get("private_network_only").(bool)),
		Datacenter:             &dc,
		NetworkComponents:      []datatypes.Virtual_Guest_Network_Component{networkComponent},
		BlockDevices:           getBlockDevicesCustom(d),
		LocalDiskFlag:          sl.Bool(d.Get("local_disk").(bool)),
		PostInstallScriptUri:   sl.String(d.Get("post_install_script_uri").(string)),
	}

	if startCPUs, ok := d.GetOk("cores"); ok {
		opts.StartCpus = sl.Int(startCPUs.(int))
	}
	if maxMemory, ok := d.GetOk("memory"); ok {
		opts.MaxMemory = sl.Int(maxMemory.(int))
	}

	if flavor, ok := d.GetOk("flavor_key_name"); ok {
		flavorComponenet := datatypes.Virtual_Guest_SupplementalCreateObjectOptions{
			FlavorKeyName: sl.String(flavor.(string)),
		}
		opts.SupplementalCreateObjectOptions = &flavorComponenet
	}

	if dedicatedAcctHostOnly, ok := d.GetOk("dedicated_acct_host_only"); ok {
		opts.DedicatedAccountHostOnlyFlag = sl.Bool(dedicatedAcctHostOnly.(bool))
	} else if dedicatedHostID, ok := d.GetOk("dedicated_host_id"); ok {
		opts.DedicatedHost = &datatypes.Virtual_DedicatedHost{
			Id: sl.Int(dedicatedHostID.(int)),
		}
	} else if dedicatedHostName, ok := d.GetOk("dedicated_host_name"); ok {
		hostName := dedicatedHostName.(string)
		service := services.GetAccountService(meta.(ClientSession).SoftLayerSession())
		hosts, err := service.
			Mask("id").
			Filter(filter.Path("dedicatedHosts.name").Eq(hostName).Build()).
			GetDedicatedHosts()

		if err != nil {
			return opts, fmt.Errorf("Error looking up dedicated host '%s': %s", hostName, err)
		} else if len(hosts) == 0 {
			return opts, fmt.Errorf("Error looking up dedicated host '%s'", hostName)
		}

		opts.DedicatedHost = &hosts[0]
	}

	if imgID, ok := d.GetOk("image_id"); ok {
		imageID := imgID.(int)
		service := services.
			GetVirtualGuestBlockDeviceTemplateGroupService(meta.(ClientSession).SoftLayerSession())

		image, err := service.
			Mask("id,globalIdentifier").Id(imageID).
			GetObject()
		if err != nil {
			return opts, fmt.Errorf("Error looking up image %d: %s", imageID, err)
		} else if image.GlobalIdentifier == nil {
			return opts, fmt.Errorf(
				"Image template %d does not have a global identifier", imageID)
		}

		opts.BlockDeviceTemplateGroup = &datatypes.Virtual_Guest_Block_Device_Template_Group{
			GlobalIdentifier: image.GlobalIdentifier,
		}
	}

	if operatingSystemReferenceCode, ok := d.GetOk("os_reference_code"); ok {
		opts.OperatingSystemReferenceCode = sl.String(operatingSystemReferenceCode.(string))
	}

	publicVlanID := d.Get("public_vlan_id").(int)
	publicSubnet := d.Get("public_subnet").(string)
	privateVlanID := d.Get("private_vlan_id").(int)
	privateSubnet := d.Get("private_subnet").(string)

	primaryNetworkComponent := datatypes.Virtual_Guest_Network_Component{
		NetworkVlan: &datatypes.Network_Vlan{},
	}

	usePrimaryNetworkComponent := false

	if publicVlanID > 0 {
		primaryNetworkComponent.NetworkVlan.Id = &publicVlanID
		usePrimaryNetworkComponent = true
	}

	// Apply public subnet if provided
	if publicSubnet != "" {
		primarySubnetID, err := getSubnetIDCustom(publicSubnet, meta)
		if err != nil {
			return opts, fmt.Errorf("Error creating virtual guest: %s", err)
		}
		primaryNetworkComponent.NetworkVlan.PrimarySubnetId = &primarySubnetID
		usePrimaryNetworkComponent = true
	}

	// Apply security groups if provided
	publicSecurityGroupIDList := d.Get("public_security_group_ids").(*schema.Set).List()
	sgb, err := expandSecurityGroupBindingsCustom(publicSecurityGroupIDList)
	if err != nil {
		return opts, err
	}
	if sgb != nil {
		primaryNetworkComponent.SecurityGroupBindings = sgb
		usePrimaryNetworkComponent = true
	}

	if usePrimaryNetworkComponent {
		opts.PrimaryNetworkComponent = &primaryNetworkComponent
	}

	primaryBackendNetworkComponent := datatypes.Virtual_Guest_Network_Component{
		NetworkVlan: &datatypes.Network_Vlan{},
	}

	usePrimaryBackendNetworkComponent := false

	if privateVlanID > 0 {
		primaryBackendNetworkComponent.NetworkVlan.Id = &privateVlanID
		usePrimaryBackendNetworkComponent = true
	}

	// Apply private subnet if provided
	if privateSubnet != "" {
		primarySubnetID, err := getSubnetIDCustom(privateSubnet, meta)
		if err != nil {
			return opts, fmt.Errorf("Error creating virtual guest: %s", err)
		}
		primaryBackendNetworkComponent.NetworkVlan.PrimarySubnetId = &primarySubnetID
		usePrimaryBackendNetworkComponent = true
	}

	// Apply security groups if provided
	privateSecurityGroupIDList := d.Get("private_security_group_ids").(*schema.Set).List()
	sgb, err = expandSecurityGroupBindingsCustom(privateSecurityGroupIDList)
	if err != nil {
		return opts, err
	}
	if sgb != nil {
		primaryBackendNetworkComponent.SecurityGroupBindings = sgb
		usePrimaryBackendNetworkComponent = true
	}

	if usePrimaryBackendNetworkComponent {
		opts.PrimaryBackendNetworkComponent = &primaryBackendNetworkComponent
	}

	if userData, ok := d.GetOk("user_metadata"); ok {
		opts.UserData = []datatypes.Virtual_Guest_Attribute{
			{
				Value: sl.String(userData.(string)),
			},
		}
	}

	// Get configured ssh_keys
	sshKeySet := d.Get("ssh_key_ids").(*schema.Set)
	sshKeys := sshKeySet.List()
	sshKeyLen := len(sshKeys)
	if sshKeyLen > 0 {
		opts.SshKeys = make([]datatypes.Security_Ssh_Key, 0, sshKeyLen)
		for _, sshKey := range sshKeys {
			opts.SshKeys = append(opts.SshKeys, datatypes.Security_Ssh_Key{
				Id: sl.Int(sshKey.(int)),
			})
		}
	}

	return opts, nil
}

func resourceIBMComputeCustomVmInstanceCreate(d *schema.ResourceData, meta interface{}) error {

	sess := meta.(ClientSession).SoftLayerSession()
	service := services.GetVirtualGuestService(sess)

	opts, err := getVirtualGuestTemplateFromResourceDataCustom(d, meta)
	if err != nil {
		return err
	}

	log.Println("[INFO] Creating virtual machine")

	var id int
	var template datatypes.Container_Product_Order

	// Build an order template with a custom image.
	if opts.BlockDevices != nil && opts.BlockDeviceTemplateGroup != nil {
		bd := *opts.BlockDeviceTemplateGroup
		opts.BlockDeviceTemplateGroup = nil
		opts.OperatingSystemReferenceCode = sl.String("UBUNTU_LATEST")
		template, err = service.GenerateOrderTemplate(&opts)
		if err != nil {
			return fmt.Errorf("Error generating order template: %s", err)
		}

		// Remove temporary OS from actual order
		prices := make([]datatypes.Product_Item_Price, len(template.Prices))
		i := 0
		for _, p := range template.Prices {
			if !strings.Contains(*p.Item.Description, "Ubuntu") {
				prices[i] = p
				i++
			}
		}
		template.Prices = prices[:i]

		template.ImageTemplateId = sl.Int(d.Get("image_id").(int))
		template.VirtualGuests[0].BlockDeviceTemplateGroup = &bd
		template.VirtualGuests[0].OperatingSystemReferenceCode = nil
	} else {
		// Build an order template with os_reference_code
		template, err = service.GenerateOrderTemplate(&opts)
		if err != nil {
			return fmt.Errorf("Error generating order template: %s", err)
		}
	}

	items, err := product.GetPackageProducts(sess, *template.PackageId, productItemMaskWithPriceLocationGroupID)
	if err != nil {
		return fmt.Errorf("Error generating order template: %s", err)
	}

	privateNetworkOnly := d.Get("private_network_only").(bool)

	secondaryIPCount := d.Get("secondary_ip_count").(int)
	if secondaryIPCount > 0 {
		if privateNetworkOnly {
			return fmt.Errorf("Unable to configure public secondary addresses with a private_network_only option")
		}
		keyName := strconv.Itoa(secondaryIPCount) + "_PUBLIC_IP_ADDRESSES"
		price, err := getItemPriceId(items, "sec_ip_addresses", keyName)
		if err != nil {
			return err
		}
		template.Prices = append(template.Prices, price)
	}

	if d.Get("ipv6_enabled").(bool) {
		if privateNetworkOnly {
			return fmt.Errorf("Unable to configure a public IPv6 address with a private_network_only option")
		}
		price, err := getItemPriceId(items, "pri_ipv6_addresses", "1_IPV6_ADDRESS")
		if err != nil {
			return fmt.Errorf("Error generating order template: %s", err)
		}
		template.Prices = append(template.Prices, price)
	}

	if d.Get("ipv6_static_enabled").(bool) {
		if privateNetworkOnly {
			return fmt.Errorf("Unable to configure a public static IPv6 address with a private_network_only option")
		}
		price, err := getItemPriceId(items, "static_ipv6_addresses", "64_BLOCK_STATIC_PUBLIC_IPV6_ADDRESSES")
		if err != nil {
			return fmt.Errorf("Error generating order template: %s", err)
		}
		template.Prices = append(template.Prices, price)
	}

	// Add optional price ids.
	// Add public bandwidth limited
	if publicBandwidth, ok := d.GetOk("public_bandwidth_limited"); ok {
		if *opts.HourlyBillingFlag {
			return fmt.Errorf("Unable to configure a public bandwidth with a hourly_billing true")
		}
		// Remove Default bandwidth price
		prices := make([]datatypes.Product_Item_Price, len(template.Prices))
		i := 0
		for _, p := range template.Prices {
			item := p.Item
			if item != nil {
				if strings.Contains(*item.Description, "Bandwidth") {
					continue
				}
			}
			prices[i] = p
			i++
		}
		template.Prices = prices[:i]
		keyName := "BANDWIDTH_" + strconv.Itoa(publicBandwidth.(int)) + "_GB"
		price, err := getItemPriceId(items, "bandwidth", keyName)
		if err != nil {
			return fmt.Errorf("Error generating order template: %s", err)
		}
		template.Prices = append(template.Prices, price)
	}

	// Add public bandwidth unlimited
	publicUnlimitedBandwidth := d.Get("public_bandwidth_unlimited").(bool)
	if publicUnlimitedBandwidth {
		if *opts.HourlyBillingFlag {
			return fmt.Errorf("Unable to configure a public bandwidth with a hourly_billing true")
		}
		networkSpeed := d.Get("network_speed").(int)
		if networkSpeed != 100 {
			return fmt.Errorf("Network speed must be 100 Mbps to configure public bandwidth unlimited")
		}
		// Remove Default bandwidth price
		prices := make([]datatypes.Product_Item_Price, len(template.Prices))
		i := 0
		for _, p := range template.Prices {
			item := p.Item
			if item != nil {
				if strings.Contains(*item.Description, "Bandwidth") {
					continue
				}
			}
			prices[i] = p
			i++
		}
		template.Prices = prices[:i]
		price, err := getItemPriceId(items, "bandwidth", "BANDWIDTH_UNLIMITED_100_MBPS_UPLINK")
		if err != nil {
			return fmt.Errorf("Error generating order template: %s", err)
		}
		template.Prices = append(template.Prices, price)
	}
	// GenerateOrderTemplate omits UserData, subnet, and maxSpeed, so configure virtual_guest.
	template.VirtualGuests[0] = opts

	order := &datatypes.Container_Product_Order_Virtual_Guest{
		Container_Product_Order_Hardware_Server: datatypes.Container_Product_Order_Hardware_Server{Container_Product_Order: template},
	}

	if opts.DedicatedHost != nil {
		order.HostId = opts.DedicatedHost.Id
	}
	orderService := services.GetProductOrderService(sess.SetRetries(0))
	receipt, err := orderService.PlaceOrder(order, sl.Bool(false))
	if err != nil {
		return fmt.Errorf("Error ordering virtual guest: %s", err)
	}
	id = *receipt.OrderDetails.VirtualGuests[0].Id

	d.SetId(fmt.Sprintf("%d", id))

	log.Printf("[INFO] Virtual Machine ID: %s", d.Id())

	// Set tags
	tags := getTagsCustom(d)
	if tags != "" {
		//Try setting only when it is non empty as we are creating virtual guest
		err = setGuestTagsCustom(id, tags, meta)
		if err != nil {
			return err
		}
	}

	var storageCustomIds []int
	if fileStorageSet := d.Get("file_storage_ids").(*schema.Set); len(fileStorageSet.List()) > 0 {
		storageCustomIds = expandIntList(fileStorageSet.List())

	}
	if blockStorageSet := d.Get("block_storage_ids").(*schema.Set); len(blockStorageSet.List()) > 0 {
		storageCustomIds = append(storageCustomIds, expandIntList(blockStorageSet.List())...)
	}
	if len(storageCustomIds) > 0 {
		err := addAccessToStorageListCustom(service.Id(id), id, storageCustomIds, meta)
		if err != nil {
			return err
		}
	}

	// Set notes
	err = setNotesCustom(id, d, meta)
	if err != nil {
		return err
	}

	// wait for machine availability

	_, err = WaitForVirtualGuestAvailableCustom(d, meta)

	if err != nil {
		return fmt.Errorf(
			"Error waiting for virtual machine (%s) to become ready: %s", d.Id(), err)
	}

	return resourceIBMComputeCustomVmInstanceRead(d, meta)
}

func resourceIBMComputeCustomVmInstanceRead(d *schema.ResourceData, meta interface{}) error {
	service := services.GetVirtualGuestService(meta.(ClientSession).SoftLayerSession())

	id, err := strconv.Atoi(d.Id())
	if err != nil {
		return fmt.Errorf("Not a valid ID, must be an integer: %s", err)
	}

	result, err := service.Id(id).Mask(
		"hostname,domain,blockDevices[diskImage],startCpus,maxMemory,dedicatedAccountHostOnlyFlag,operatingSystemReferenceCode,blockDeviceTemplateGroup[id]," +
			"billingItem[orderItem[preset[keyName]]]," +
			"primaryIpAddress,primaryBackendIpAddress,privateNetworkOnlyFlag," +
			"hourlyBillingFlag,localDiskFlag," +
			"allowedNetworkStorage[id,nasType]," +
			"notes,userData[value],tagReferences[id,tag[name]]," +
			"datacenter[id,name,longName]," +
			"sshKeys," +
			"primaryNetworkComponent[networkVlan[id],subnets," +
			"primaryVersion6IpAddressRecord[subnet,guestNetworkComponentBinding[ipAddressId]]," +
			"primaryIpAddressRecord[subnet,guestNetworkComponentBinding[ipAddressId]]," +
			"securityGroupBindings[securityGroup]]," +
			"softwareComponents[passwords[password]]," +
			"primaryBackendNetworkComponent[networkVlan[id]," +
			"primaryIpAddressRecord[subnet,guestNetworkComponentBinding[ipAddressId]]," +
			"securityGroupBindings[securityGroup]]",
	).GetObject()

	if err != nil {
		return fmt.Errorf("Error retrieving virtual guest: %s", err)
	}

	d.Set("hostname", *result.Hostname)
	d.Set("domain", *result.Domain)

	if result.BillingItem.OrderItem.Preset != nil {

		d.Set("flavor_key_name", *result.BillingItem.OrderItem.Preset.KeyName)
	}

	if result.BlockDeviceTemplateGroup != nil {
		d.Set("image_id", result.BlockDeviceTemplateGroup.Id)
	} else {
		//Provided only for the sake of importing os_reference_Code
		//In other flows when user gives say UBUNTU_LATEST in the configuration file, the value read back from API might be UBUNTU_16_64
		//which is the actual Ubuntu version which gets provisioned. So we simply avoid writing back the value received to avoid creating diff
		if _, ok := d.GetOk("os_reference_code"); !ok {
			d.Set("os_reference_code", result.OperatingSystemReferenceCode)
		}
	}

	if result.Datacenter != nil {
		d.Set("datacenter", *result.Datacenter.Name)
	}

	if result.DedicatedHost != nil {
		d.Set("dedicated_host_id", *result.DedicatedHost.Id)
		d.Set("dedicated_host_name", *result.DedicatedHost.Name)
	}

	d.Set(
		"network_speed",
		sl.Grab(
			result,
			"PrimaryBackendNetworkComponent.MaxSpeed",
			d.Get("network_speed").(int),
		),
	)
	if result.OperatingSystemReferenceCode != nil && strings.HasPrefix(*result.OperatingSystemReferenceCode, "WIN") {
		d.Set("disks", flattenDisksForWindows(result))
	} else {
		d.Set("disks", flattenDisks(result))
	}
	d.Set("cores", *result.StartCpus)
	d.Set("memory", *result.MaxMemory)
	d.Set("dedicated_acct_host_only", *result.DedicatedAccountHostOnlyFlag)
	if result.PrimaryIpAddress != nil {
		d.Set("has_public_ip", *result.PrimaryIpAddress != "")
	}
	d.Set("ipv4_address", result.PrimaryIpAddress)
	d.Set("ipv4_address_private", result.PrimaryBackendIpAddress)
	if result.PrimaryNetworkComponent.PrimaryIpAddressRecord != nil {
		d.Set("ip_address_id", *result.PrimaryNetworkComponent.PrimaryIpAddressRecord.GuestNetworkComponentBinding.IpAddressId)
	}
	d.Set("public_interface_id", result.PrimaryNetworkComponent.Id)
	d.Set("ip_address_id_private",
		*result.PrimaryBackendNetworkComponent.PrimaryIpAddressRecord.GuestNetworkComponentBinding.IpAddressId)
	d.Set("private_interface_id", result.PrimaryBackendNetworkComponent.Id)
	d.Set("private_network_only", *result.PrivateNetworkOnlyFlag)
	d.Set("hourly_billing", *result.HourlyBillingFlag)
	d.Set("local_disk", *result.LocalDiskFlag)
	d.Set("secret", result.SoftwareComponents[0].Passwords[0].Password)
	if result.PrimaryNetworkComponent.NetworkVlan != nil {
		d.Set("public_vlan_id", *result.PrimaryNetworkComponent.NetworkVlan.Id)
	}

	d.Set("private_vlan_id", *result.PrimaryBackendNetworkComponent.NetworkVlan.Id)

	if result.PrimaryNetworkComponent.PrimaryIpAddressRecord != nil {
		publicSubnet := result.PrimaryNetworkComponent.PrimaryIpAddressRecord.Subnet
		d.Set(
			"public_subnet",
			fmt.Sprintf("%s/%d", *publicSubnet.NetworkIdentifier, *publicSubnet.Cidr),
		)
		d.Set("public_subnet_id", result.PrimaryNetworkComponent.PrimaryIpAddressRecord.SubnetId)
	}

	if result.PrimaryNetworkComponent.SecurityGroupBindings != nil {
		var sgs []int
		for _, sg := range result.PrimaryNetworkComponent.SecurityGroupBindings {
			sgs = append(sgs, *sg.SecurityGroup.Id)
		}
		d.Set("public_security_group_ids", sgs)
	}

	privateSubnet := result.PrimaryBackendNetworkComponent.PrimaryIpAddressRecord.Subnet
	d.Set(
		"private_subnet",
		fmt.Sprintf("%s/%d", *privateSubnet.NetworkIdentifier, *privateSubnet.Cidr),
	)
	d.Set("private_subnet_id", result.PrimaryBackendNetworkComponent.PrimaryIpAddressRecord.SubnetId)

	if result.PrimaryBackendNetworkComponent.SecurityGroupBindings != nil {
		var sgs []int
		for _, sg := range result.PrimaryBackendNetworkComponent.SecurityGroupBindings {
			sgs = append(sgs, *sg.SecurityGroup.Id)
		}
		d.Set("private_security_group_ids", sgs)
	}

	d.Set("ipv6_enabled", false)
	d.Set("ipv6_static_enabled", false)
	if result.PrimaryNetworkComponent.PrimaryVersion6IpAddressRecord != nil {
		d.Set("ipv6_enabled", true)
		d.Set("ipv6_address", *result.PrimaryNetworkComponent.PrimaryVersion6IpAddressRecord.IpAddress)
		d.Set("ipv6_address_id", *result.PrimaryNetworkComponent.PrimaryVersion6IpAddressRecord.GuestNetworkComponentBinding.IpAddressId)
		publicSubnet := result.PrimaryNetworkComponent.PrimaryVersion6IpAddressRecord.Subnet
		log.Println("DUDE", *publicSubnet, result.PrimaryNetworkComponent.PrimaryVersion6IpAddressRecord.SubnetId)
		d.Set(
			"public_ipv6_subnet",
			fmt.Sprintf("%s/%d", *publicSubnet.NetworkIdentifier, *publicSubnet.Cidr),
		)
		d.Set("public_ipv6_subnet_id", result.PrimaryNetworkComponent.PrimaryVersion6IpAddressRecord.SubnetId)
	}
	for _, subnet := range result.PrimaryNetworkComponent.Subnets {
		if *subnet.SubnetType == "STATIC_IP_ROUTED_6" {
			d.Set("ipv6_static_enabled", true)
		}
	}

	userData := result.UserData

	if userData != nil && len(userData) > 0 {
		d.Set("user_metadata", userData[0].Value)
	}

	d.Set("notes", sl.Get(result.Notes, nil))

	tagReferences := result.TagReferences
	tagReferencesLen := len(tagReferences)
	if tagReferencesLen > 0 {
		tags := make([]string, 0, tagReferencesLen)
		for _, tagRef := range tagReferences {
			tags = append(tags, *tagRef.Tag.Name)
		}
		d.Set("tags", tags)
	}

	storages := result.AllowedNetworkStorage
	d.Set("block_storage_ids", flattenBlockStorageID(storages))
	d.Set("file_storage_ids", flattenFileStorageID(storages))

	sshKeys := result.SshKeys
	if len(sshKeys) > 0 {
		d.Set("ssh_key_ids", flattenSSHKeyIDs(sshKeys))
	}

	// Set connection info
	connInfo := map[string]string{"type": "ssh"}
	if !*result.PrivateNetworkOnlyFlag && result.PrimaryIpAddress != nil {
		connInfo["host"] = *result.PrimaryIpAddress
	} else {
		connInfo["host"] = *result.PrimaryBackendIpAddress
	}
	d.SetConnInfo(connInfo)

	err = readSecondaryIPAddressesCustom(d, meta, result.PrimaryIpAddress)
	return err
}

func readSecondaryIPAddressesCustom(d *schema.ResourceData, meta interface{}, primaryIPAddress *string) error {
	d.Set("secondary_ip_addresses", nil)
	if primaryIPAddress != nil {
		secondarySubnetResult, err := services.GetAccountService(meta.(ClientSession).SoftLayerSession()).
			Mask("ipAddresses[id,ipAddress],subnetType").
			Filter(filter.Build(filter.Path("publicSubnets.endPointIpAddress.ipAddress").Eq(*primaryIPAddress))).
			GetPublicSubnets()
		if err != nil {
			log.Printf("Error getting secondary Ip addresses: %s", err)
		}

		secondaryIps := make([]string, 0)
		for _, subnet := range secondarySubnetResult {
			// Count static secondary ip addresses.
			if *subnet.SubnetType == staticIPRoutedCustom {
				for _, ipAddressObj := range subnet.IpAddresses {
					secondaryIps = append(secondaryIps, *ipAddressObj.IpAddress)
				}
			}
		}
		if len(secondaryIps) > 0 {
			d.Set("secondary_ip_addresses", secondaryIps)
			d.Set("secondary_ip_count", len(secondaryIps))
		}
	}
	return nil
}
func resourceIBMComputeCustomVmInstanceUpdate(d *schema.ResourceData, meta interface{}) error {

	sess := meta.(ClientSession).SoftLayerSession()
	service := services.GetVirtualGuestService(sess)

	id, err := strconv.Atoi(d.Id())
	if err != nil {
		return fmt.Errorf("Not a valid ID, must be an integer: %s", err)
	}

	result, err := service.Id(id).GetObject()
	if err != nil {
		return fmt.Errorf("Error retrieving virtual guest: %s", err)
	}

	isChanged := false

	// Update "hostname" and "domain" fields if present and changed
	// Those are the only fields, which could be updated
	if d.HasChange("hostname") || d.HasChange("domain") {
		result.Hostname = sl.String(d.Get("hostname").(string))
		result.Domain = sl.String(d.Get("domain").(string))
		isChanged = true
	}

	if d.HasChange("notes") {
		result.Notes = sl.String(d.Get("notes").(string))
		isChanged = true
	}

	if isChanged {
		_, err = service.Id(id).EditObject(&result)
		if err != nil {
			return fmt.Errorf("Couldn't update virtual guest: %s", err)
		}
	}

	// Update tags
	if d.HasChange("tags") {
		tags := getTagsCustom(d)
		err := setGuestTagsCustom(id, tags, meta)
		if err != nil {
			return err
		}
	}

	err = modifyStorageAccessCustom(service.Id(id), id, meta, d)
	if err != nil {
		return err
	}

	// Upgrade "cores", "memory" and "network_speed" if provided and changed
	upgradeOptions := map[string]float64{}
	if d.HasChange("cores") {
		upgradeOptions[product.CPUCategoryCode] = float64(d.Get("cores").(int))
	}

	if d.HasChange("memory") {
		memoryInMB := float64(d.Get("memory").(int))

		// Convert memory to GB, as softlayer only allows to upgrade RAM in Gigs
		// Must be already validated at this step
		upgradeOptions[product.MemoryCategoryCode] = float64(int(memoryInMB / 1024))
	}

	if d.HasChange("network_speed") {
		upgradeOptions[product.NICSpeedCategoryCode] = float64(d.Get("network_speed").(int))
	}

	if d.HasChange("disks") {
		oldDisks, newDisks := d.GetChange("disks")
		oldDisk := oldDisks.([]interface{})
		newDisk := newDisks.([]interface{})

		//Remove is not supported for now.
		if len(oldDisk) > len(newDisk) {
			return fmt.Errorf("Removing drives is not supported.")
		}

		var diskName string
		//Update the disks if any change
		for i := 0; i < len(oldDisk); i++ {
			if newDisk[i].(int) != oldDisk[i].(int) {

				if _, ok := d.GetOk("flavor_key_name"); ok {
					diskName = fmt.Sprintf("guest_disk%d", i+1)
				} else {
					diskName = fmt.Sprintf("guest_disk%d", i)
				}
				capacity := newDisk[i].(int)
				upgradeOptions[diskName] = float64(capacity)
			}
		}
		//Add new disks
		for i := len(oldDisk); i < len(newDisk); i++ {
			if _, ok := d.GetOk("flavor_key_name"); ok {
				diskName = fmt.Sprintf("guest_disk%d", i+1)
			} else {
				diskName = fmt.Sprintf("guest_disk%d", i)
			}
			capacity := newDisk[i].(int)
			upgradeOptions[diskName] = float64(capacity)
		}

	}
	if len(upgradeOptions) > 0 || d.HasChange("flavor_key_name") {

		if _, ok := d.GetOk("flavor_key_name"); ok {
			presetKeyName := d.Get("flavor_key_name").(string)
			_, err = virtual.UpgradeVirtualGuestWithPreset(sess.SetRetries(0), &result, presetKeyName, upgradeOptions)
			if err != nil {
				return fmt.Errorf("Couldn't upgrade virtual guest: %s", err)
			}

		} else {
			_, err = virtual.UpgradeVirtualGuest(sess.SetRetries(0), &result, upgradeOptions)
			if err != nil {
				return fmt.Errorf("Couldn't upgrade virtual guest: %s", err)
			}
		}

		// Wait for softlayer to start upgrading...
		_, err = WaitForUpgradeTransactionsToAppearCustom(d, meta)
		if err != nil {
			return err
		}
		// Wait for upgrade transactions to finish
		_, err = WaitForNoActiveTransactionsCustom(d, meta)
		if err != nil {
			return err
		}

	}

	return resourceIBMComputeCustomVmInstanceRead(d, meta)
}

func modifyStorageAccessCustom(sam storageAccessModifierCustom, deviceID int, meta interface{}, d *schema.ResourceData) error {
	var remove, add []int
	if d.HasChange("file_storage_ids") {
		o, n := d.GetChange("file_storage_ids")
		os := o.(*schema.Set)
		ns := n.(*schema.Set)

		remove = expandIntList(os.Difference(ns).List())
		add = expandIntList(ns.Difference(os).List())
	}
	if d.HasChange("block_storage_ids") {
		o, n := d.GetChange("block_storage_ids")
		os := o.(*schema.Set)
		ns := n.(*schema.Set)

		remove = append(remove, expandIntList(os.Difference(ns).List())...)
		add = append(add, expandIntList(ns.Difference(os).List())...)
	}

	if len(add) > 0 {
		err := addAccessToStorageListCustom(sam, deviceID, add, meta)
		if err != nil {
			return err
		}
	}
	if len(remove) > 0 {
		err := removeAccessToStorageListCustom(sam, deviceID, remove, meta)
		if err != nil {
			return err
		}
	}
	return nil
}

func resourceIBMComputeCustomVmInstanceDelete(d *schema.ResourceData, meta interface{}) error {
	sess := meta.(ClientSession).SoftLayerSession()
	service := services.GetVirtualGuestService(sess)

	id, err := strconv.Atoi(d.Id())
	if err != nil {
		return fmt.Errorf("Not a valid ID, must be an integer: %s", err)
	}

	_, err = WaitForNoActiveTransactionsCustom(d, meta)

	if err != nil {
		return fmt.Errorf("Error deleting virtual guest, couldn't wait for zero active transactions: %s", err)
	}
	err = detachSecurityGroupNetworkComponentBindingsCustom(d, meta, id)
	if err != nil {
		return err
	}
	ok, err := service.Id(id).DeleteObject()
	if err != nil {
		return fmt.Errorf("Error deleting virtual guest: %s", err)
	}

	if !ok {
		return fmt.Errorf(
			"API reported it was unsuccessful in removing the virtual guest '%d'", id)
	}

	return nil
}

func detachSecurityGroupNetworkComponentBindingsCustom(d *schema.ResourceData, meta interface{}, id int) error {
	sess := meta.(ClientSession).SoftLayerSession()
	service := services.GetVirtualGuestService(sess)
	publicSgIDs := d.Get("public_security_group_ids").(*schema.Set).List()
	privateSgIDS := d.Get("private_security_group_ids").(*schema.Set).List()
	if len(publicSgIDs) == 0 && len(privateSgIDS) == 0 {
		log.Println("No security groups specified, hence no detachment required before delete operation")
		return nil
	}
	vsi, err := service.Id(id).Mask(
		"primaryNetworkComponent[id,securityGroupBindings[securityGroupId,networkComponentId]]," +
			"primaryBackendNetworkComponent[id,securityGroupBindings[securityGroupId,networkComponentId]]",
	).GetObject()

	if err != nil {
		return err
	}
	sgService := services.GetNetworkSecurityGroupService(sess)
	//Detach security group as destroy might fail if the security group is attempted
	//to be destroyed in the same terraform configuration file. VSI destroy takes
	//some time andif during the same time security group which was referred in the VSI
	//is attempted to be destroyed it will fail.
	for _, v := range publicSgIDs {
		sgID := v.(int)
		for _, v := range vsi.PrimaryNetworkComponent.SecurityGroupBindings {
			if sgID == *v.SecurityGroupId {
				_, err := sgService.Id(sgID).DetachNetworkComponents([]int{*v.NetworkComponentId})
				if err != nil {
					return err
				}
			}
		}
	}
	for _, v := range privateSgIDS {
		sgID := v.(int)
		for _, v := range vsi.PrimaryBackendNetworkComponent.SecurityGroupBindings {
			if sgID == *v.SecurityGroupId {
				_, err := sgService.Id(sgID).DetachNetworkComponents([]int{*v.NetworkComponentId})
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

//genIDCustom generates a random string to be used for the optional
//hostname
func genIDCustom() (interface{}, error) {
	numBytes := 8
	bytes := make([]byte, numBytes)
	n, err := rand.Reader.Read(bytes)
	if err != nil {
		return nil, err
	}

	if n != numBytes {
		return nil, errors.New("generated insufficient random bytes")
	}

	hexStr := hex.EncodeToString(bytes)
	return fmt.Sprintf("terraformed-%s", hexStr), nil
}

// WaitForUpgradeTransactionsToAppearCustom Wait for upgrade transactions
func WaitForUpgradeTransactionsToAppearCustom(d *schema.ResourceData, meta interface{}) (interface{}, error) {
	log.Printf("Waiting for server (%s) to have upgrade transactions", d.Id())

	id, err := strconv.Atoi(d.Id())
	if err != nil {
		return nil, fmt.Errorf("The instance ID %s must be numeric", d.Id())
	}
	stateConf := &resource.StateChangeConf{
		Pending: []string{"retry", pendingUpgradeCustom},
		Target:  []string{inProgressUpgradeCustom},
		Refresh: func() (interface{}, string, error) {
			service := services.GetVirtualGuestService(meta.(ClientSession).SoftLayerSession())
			transactions, err := service.Id(id).GetActiveTransactions()
			if err != nil {
				if apiErr, ok := err.(sl.Error); ok && apiErr.StatusCode == 404 {
					return nil, "", fmt.Errorf("Couldn't fetch active transactions: %s", err)
				}
				return false, "retry", nil
			}
			for _, transaction := range transactions {
				if strings.Contains(*transaction.TransactionStatus.Name, upgradeTransactionCustom) {
					return transactions, inProgressUpgradeCustom, nil
				}
			}
			return transactions, pendingUpgradeCustom, nil
		},
		Timeout:    10 * time.Minute,
		Delay:      5 * time.Second,
		MinTimeout: 5 * time.Second,
	}

	return stateConf.WaitForState()
}

// WaitForNoActiveTransactionsCustom Wait for no active transactions
func WaitForNoActiveTransactionsCustom(d *schema.ResourceData, meta interface{}) (interface{}, error) {
	log.Printf("Waiting for server (%s) to have zero active transactions", d.Id())
	id, err := strconv.Atoi(d.Id())
	if err != nil {
		return nil, fmt.Errorf("The instance ID %s must be numeric", d.Id())
	}
	stateConf := &resource.StateChangeConf{
		Pending: []string{"retry", activeTransaction},
		Target:  []string{idleTransactionCustom},
		Refresh: func() (interface{}, string, error) {
			service := services.GetVirtualGuestService(meta.(ClientSession).SoftLayerSession())
			transactions, err := service.Id(id).GetActiveTransactions()
			if err != nil {
				if apiErr, ok := err.(sl.Error); ok && apiErr.StatusCode == 404 {
					return nil, "", fmt.Errorf("Couldn't get active transactions: %s", err)
				}
				return false, "retry", nil
			}
			if len(transactions) == 0 {
				return transactions, idleTransactionCustom, nil
			}
			return transactions, activeTransaction, nil
		},
		Timeout:    time.Duration(d.Get("wait_time_minutes").(int)) * time.Minute,
		Delay:      10 * time.Second,
		MinTimeout: 10 * time.Second,
	}

	return stateConf.WaitForState()
}

// WaitForVirtualGuestAvailableCustom Waits for virtual guest creation
func WaitForVirtualGuestAvailableCustom(d *schema.ResourceData, meta interface{}) (interface{}, error) {
	log.Printf("Waiting for server (%s) to be available.", d.Id())
	id, err := strconv.Atoi(d.Id())
	if err != nil {
		return nil, fmt.Errorf("The instance ID %s must be numeric", d.Id())
	}
	sess := meta.(ClientSession).SoftLayerSession()
	stateConf := &resource.StateChangeConf{
		Pending:    []string{"retry", virtualGuestProvisioningCustom},
		Target:     []string{virtualGuestAvailableCustom},
		Refresh:    virtualGuestStateRefreshFuncCustom(sess, id, d),
		Timeout:    time.Duration(d.Get("wait_time_minutes").(int)) * time.Minute,
		Delay:      10 * time.Second,
		MinTimeout: 10 * time.Second,
	}

	return stateConf.WaitForState()
}

func virtualGuestStateRefreshFuncCustom(sess *session.Session, instanceID int, d *schema.ResourceData) resource.StateRefreshFunc {
	return func() (interface{}, string, error) {
		// Check active transactions
		publicNetwork := !d.Get("private_network_only").(bool)
		service := services.GetVirtualGuestService(sess)
		result, err := service.Id(instanceID).Mask("activeTransaction,primaryBackendIpAddress,primaryIpAddress").GetObject()
		if err != nil {
			if apiErr, ok := err.(sl.Error); ok && apiErr.StatusCode == 404 {
				return nil, "", fmt.Errorf("Error retrieving virtual guest: %s", err)
			}
			return false, "retry", nil
		}
		// Check active transactions
		log.Println("Checking active transactions.")
		if result.ActiveTransaction != nil {
			return result, virtualGuestProvisioningCustom, nil
		}

		// Check Primary IP address availability.
		log.Println("Checking primary backend IP address.")
		if result.PrimaryBackendIpAddress == nil {
			return result, virtualGuestProvisioningCustom, nil
		}

		log.Println("Checking primary IP address.")
		if publicNetwork && result.PrimaryIpAddress == nil {
			return result, virtualGuestProvisioningCustom, nil
		}

		// Check Secondary IP address availability.
		if d.Get("secondary_ip_count").(int) > 0 {
			log.Println("Refreshing secondary IPs state.")
			secondarySubnetResult, err := services.GetAccountService(sess).
				Mask("ipAddresses[id,ipAddress]").
				Filter(filter.Build(filter.Path("publicSubnets.endPointIpAddress.virtualGuest.id").Eq(d.Id()))).
				GetPublicSubnets()
			if err != nil {
				return nil, "", fmt.Errorf("Error retrieving secondary ip address: %s", err)
			}
			if len(secondarySubnetResult) == 0 {
				return result, virtualGuestProvisioningCustom, nil
			}
		}

		return result, virtualGuestAvailableCustom, nil
	}
}

func resourceIBMComputeCustomVmInstanceExists(d *schema.ResourceData, meta interface{}) (bool, error) {
	service := services.GetVirtualGuestService(meta.(ClientSession).SoftLayerSession())
	guestID, err := strconv.Atoi(d.Id())
	if err != nil {
		return false, fmt.Errorf("Not a valid ID, must be an integer: %s", err)
	}

	result, err := service.Id(guestID).GetObject()
	if err != nil {
		if apiErr, ok := err.(sl.Error); ok {
			if apiErr.StatusCode == 404 {
				return false, nil
			}
		}
		return false, fmt.Errorf("Error communicating with the API: %s", err)
	}

	return result.Id != nil && *result.Id == guestID, nil
}

func getTagsCustom(d dataRetriever) string {
	tagSet := d.Get("tags").(*schema.Set)

	if tagSet.Len() == 0 {
		return ""
	}

	tags := make([]string, 0, tagSet.Len())
	for _, elem := range tagSet.List() {
		tag := elem.(string)
		tags = append(tags, tag)
	}
	return strings.Join(tags, ",")
}

func setGuestTagsCustom(id int, tags string, meta interface{}) error {
	service := services.GetVirtualGuestService(meta.(ClientSession).SoftLayerSession())
	_, err := service.Id(id).SetTags(sl.String(tags))
	if err != nil {
		return fmt.Errorf("Could not set tags on virtual guest %d", id)
	}
	return nil
}

type storageAccessModifierCustom interface {
	AllowAccessToNetworkStorageList([]datatypes.Network_Storage) (resp bool, err error)
	RemoveAccessToNetworkStorageList([]datatypes.Network_Storage) (resp bool, err error)
}

func addAccessToStorageListCustom(sam storageAccessModifierCustom, deviceID int, ids storageCustomIds, meta interface{}) error {
	s, err := ids.Storages(meta)
	if err != nil {
		return err
	}
	for {
		_, err := sam.AllowAccessToNetworkStorageList(s)
		if err != nil {
			if apiErr, ok := err.(sl.Error); ok && apiErr.Exception == networkStorageMassAccessControlModificationExceptionCustom {
				log.Printf("[DEBUG]  Allow access to storage failed with error %q. Will retry again after %q", err, retryDelayForModifyingStorageAccessCustom)
				time.Sleep(retryDelayForModifyingStorageAccessCustom)
				continue
			}
			return fmt.Errorf("Could not authorize Device %d, access to the following storages %q, %q", deviceID, ids, err)
		}
		log.Printf("[INFO] Device authorized to access %q", ids)
		break
	}
	return nil
}

func removeAccessToStorageListCustom(sam storageAccessModifierCustom, deviceID int, ids storageCustomIds, meta interface{}) error {
	s, err := ids.Storages(meta)
	if err != nil {
		return err
	}
	for {
		_, err := sam.RemoveAccessToNetworkStorageList(s)
		if err != nil {
			if apiErr, ok := err.(sl.Error); ok && apiErr.Exception == networkStorageMassAccessControlModificationExceptionCustom {
				log.Printf("[DEBUG]  Remove access to storage failed with error %q. Will retry again after %q", err, retryDelayForModifyingStorageAccessCustom)
				time.Sleep(retryDelayForModifyingStorageAccessCustom)
				continue
			}
			return fmt.Errorf("Could not remove Device %d, access to the following storages %q, %q", deviceID, ids, err)
		}
		log.Printf("[INFO] Devices's access to %q have been removed", ids)
		break
	}
	return nil
}

func setNotesCustom(id int, d *schema.ResourceData, meta interface{}) error {
	sess := meta.(ClientSession).SoftLayerSession()
	service := services.GetVirtualGuestService(sess)

	if notes := d.Get("notes").(string); notes != "" {
		result, err := service.Id(id).GetObject()
		if err != nil {
			return fmt.Errorf("Error retrieving virtual guest: %s", err)
		}

		result.Notes = sl.String(notes)

		_, err = service.Id(id).EditObject(&result)
		if err != nil {
			return fmt.Errorf("Could not set note on virtual guest %d", id)
		}
	}

	return nil
}
