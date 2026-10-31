// SPDX-License-Identifier: GPL-2.0
/*
 * Pixel pKVM (android14-6.1 hosts) never unmaps a guest page: the stage-2
 * entry stays on the host page the guest first touched. When crosvm maps a
 * new virtio-gpu blob where an old one was, the guest would keep reaching
 * the old one. Relinquishing a host-visible range before a blob is mapped
 * there drops those entries, and the next access faults onto the new blob.
 * A hypervisor that unmaps on its own makes this a no-op.
 */
#include <linux/arm-smccc.h>
#include <linux/kprobes.h>
#include <linux/module.h>
#include <asm/hypervisor.h>

#include "virtgpu_drv.h"

/* Debug counters; racy increments are fine. */
static long maps, pages, refused;
module_param(maps, long, 0444);
module_param(pages, long, 0444);
module_param(refused, long, 0444);

/* virtio_gpu_cmd_map(vgdev, objs, offset): offset is into the host-visible window. */
static int pre_cmd_map(struct kprobe *p, struct pt_regs *regs)
{
	struct virtio_gpu_device *vgdev = (void *)regs->regs[0];
	struct virtio_gpu_object_array *objs = (void *)regs->regs[1];
	u64 offset = regs->regs[2];
	phys_addr_t phys = vgdev->host_visible_region.addr + offset;
	phys_addr_t end = phys + objs->objs[0]->size;
	struct arm_smccc_res res;
	long n = 0, bad = 0;

	for (; phys < end; phys += PAGE_SIZE, n++) {
		arm_smccc_1_1_invoke(ARM_SMCCC_VENDOR_HYP_KVM_MEM_RELINQUISH_FUNC_ID, phys, 0, 0, &res);
		if (res.a0 != SMCCC_RET_SUCCESS)
			bad++;
	}
	maps++;
	pages += n;
	refused += bad;
	if (bad)
		pr_warn_ratelimited("vgpu_relinquish: %ld of %ld pages refused at offset %#llx\n", bad, n, offset);
	return 0;
}

static struct kprobe kp = {
	.symbol_name = "virtio_gpu_cmd_map",
	.pre_handler = pre_cmd_map,
};
static bool registered;

static int __init vgpu_relinquish_init(void)
{
	int ret;

	/* Not under pKVM (QEMU, other hypervisors): there's nothing to drop. */
	if (!kvm_arm_hyp_service_available(ARM_SMCCC_KVM_FUNC_MEM_RELINQUISH)) {
		pr_info("vgpu_relinquish: the hypervisor has no MEM_RELINQUISH; idle\n");
		return 0;
	}
	ret = register_kprobe(&kp);
	if (ret) {
		pr_err("vgpu_relinquish: register_kprobe: %d\n", ret);
		return ret;
	}
	registered = true;
	return 0;
}

static void __exit vgpu_relinquish_exit(void)
{
	if (registered)
		unregister_kprobe(&kp);
}

module_init(vgpu_relinquish_init);
module_exit(vgpu_relinquish_exit);
MODULE_LICENSE("GPL");
MODULE_DESCRIPTION("Relinquish virtio-gpu host-visible pages before a blob is mapped (Pixel pKVM)");
